package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// RunQueuedChannel is the PostgreSQL NOTIFY channel the scheduler listens on
// to pick up freshly queued runs. Payload is the run_id as a plain UUID string.
const RunQueuedChannel = "run_queued"

// CreateRunFromModificationInput bundles everything needed to spawn a run
// triggered by a matched modification (typically a webhook push). It is kept
// intentionally concrete — other trigger flows (upstream, cron, manual) will
// adapt onto a shared createRun internal once they land.
type CreateRunFromModificationInput struct {
	PipelineID     uuid.UUID
	MaterialID     uuid.UUID
	ModificationID int64
	Revision       string
	Branch         string
	Provider       string
	Delivery       string
	TriggeredBy    string
	// Cause + CauseDetail override the default cause="webhook" run
	// labelling. Empty Cause keeps the existing webhook default;
	// callers tagging a run as `pull_request` (etc.) supply both.
	// CauseDetail is merged onto the provider/delivery metadata —
	// keys in CauseDetail win if a conflict arises.
	Cause       string
	CauseDetail json.RawMessage
}

type StageRunRef struct {
	ID      uuid.UUID
	Name    string
	Ordinal int
}

type JobRunRef struct {
	ID         uuid.UUID
	StageRunID uuid.UUID
	Name       string
	MatrixKey  string
}

type RunCreated struct {
	RunID     uuid.UUID
	Counter   int64
	StageRuns []StageRunRef
	JobRuns   []JobRunRef
}

// CreateRunFromModification materializes a queued run triggered by a matched
// webhook modification. Thin adapter over insertRunSkeleton — all the heavy
// lifting (counter + stage_runs + job_runs + NOTIFY) lives there so other
// trigger paths (upstream, cron, manual) share the same insertion logic.
func (s *Store) CreateRunFromModification(ctx context.Context, in CreateRunFromModificationInput) (RunCreated, error) {
	base := map[string]any{
		"provider":        in.Provider,
		"delivery":        in.Delivery,
		"material_id":     in.MaterialID.String(),
		"modification_id": in.ModificationID,
	}
	// Caller-provided cause_detail is merged last so PR metadata (or
	// whatever other trigger adds) takes precedence on a key clash.
	if len(in.CauseDetail) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(in.CauseDetail, &extra); err == nil {
			for k, v := range extra {
				base[k] = v
			}
		}
	}
	causeDetail, _ := json.Marshal(base)
	revisions, _ := json.Marshal(map[string]any{
		in.MaterialID.String(): map[string]string{
			"revision": in.Revision,
			"branch":   in.Branch,
		},
	})
	cause := in.Cause
	if cause == "" {
		cause = string(domain.CauseWebhook)
	}
	return s.insertRunSkeleton(ctx, insertRunSkeletonInput{
		PipelineID:  in.PipelineID,
		Cause:       cause,
		CauseDetail: causeDetail,
		Revisions:   revisions,
		TriggeredBy: in.TriggeredBy,
	})
}

// insertRunSkeletonInput is the minimal payload for creating a queued run:
// whatever already-serialized cause + revisions the caller computed.
type insertRunSkeletonInput struct {
	PipelineID  uuid.UUID
	Cause       string
	CauseDetail json.RawMessage
	Revisions   json.RawMessage
	TriggeredBy string
}

// insertRunSkeleton runs the full "create run + stages + jobs + NOTIFY" dance
// inside one tx. Trigger-specific code prepares cause+revisions and calls in.
func (s *Store) insertRunSkeleton(ctx context.Context, in insertRunSkeletonInput) (RunCreated, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RunCreated{}, fmt.Errorf("store: create run: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	pipelineRow, err := q.GetPipelineDefinition(ctx, pgUUID(in.PipelineID))
	if err != nil {
		return RunCreated{}, fmt.Errorf("store: create run: load pipeline %s: %w", in.PipelineID, err)
	}

	var def domain.Pipeline
	if err := json.Unmarshal(pipelineRow.Definition, &def); err != nil {
		return RunCreated{}, fmt.Errorf("store: create run: decode definition: %w", err)
	}
	if len(def.Stages) == 0 {
		return RunCreated{}, fmt.Errorf("store: create run: pipeline %s has no stages", in.PipelineID)
	}

	counter, err := q.NextRunCounter(ctx, pipelineRow.ID)
	if err != nil {
		return RunCreated{}, fmt.Errorf("store: create run: counter: %w", err)
	}

	runRow, err := q.InsertRun(ctx, db.InsertRunParams{
		PipelineID:  pipelineRow.ID,
		Counter:     counter,
		Cause:       in.Cause,
		CauseDetail: in.CauseDetail,
		Revisions:   in.Revisions,
		TriggeredBy: nullableString(in.TriggeredBy),
	})
	if err != nil {
		return RunCreated{}, fmt.Errorf("store: insert run: %w", err)
	}

	result := RunCreated{RunID: fromPgUUID(runRow.ID), Counter: runRow.Counter}

	stageIDByName := make(map[string]uuid.UUID, len(def.Stages))
	for i, name := range def.Stages {
		row, err := q.InsertStageRun(ctx, db.InsertStageRunParams{
			RunID:   runRow.ID,
			Name:    name,
			Ordinal: int32(i),
		})
		if err != nil {
			return RunCreated{}, fmt.Errorf("store: insert stage %s: %w", name, err)
		}
		id := fromPgUUID(row.ID)
		stageIDByName[name] = id
		result.StageRuns = append(result.StageRuns, StageRunRef{ID: id, Name: name, Ordinal: i})
	}

	for _, job := range def.Jobs {
		stageID, ok := stageIDByName[job.Stage]
		if !ok {
			return RunCreated{}, fmt.Errorf("store: job %q references unknown stage %q", job.Name, job.Stage)
		}
		needs := job.Needs
		if needs == nil {
			needs = []string{}
		}
		combos := expandMatrix(job.Matrix)
		for _, combo := range combos {
			key := matrixKey(combo)
			row, err := q.InsertJobRun(ctx, db.InsertJobRunParams{
				RunID:      runRow.ID,
				StageRunID: pgUUID(stageID),
				Name:       job.Name,
				MatrixKey:  nullableString(key),
				Image:      nullableString(job.Image),
				Needs:      needs,
			})
			if err != nil {
				return RunCreated{}, fmt.Errorf("store: insert job %s[%s]: %w", job.Name, key, err)
			}
			// Approval gates get the same INSERT as regular jobs
			// then a follow-up UPDATE that stamps the gate fields
			// + flips status to awaiting_approval. Keeping this
			// out of InsertJobRun avoids regenerating sqlc for a
			// niche branch; the UPDATE runs in the same tx so a
			// crash between INSERT and UPDATE rolls everything
			// back. The scheduler's dispatch query filters on
			// status='queued' so these rows are invisible to it
			// until someone approves.
			if job.Approval != nil {
				approvers := job.Approval.Approvers
				if approvers == nil {
					approvers = []string{}
				}
				if _, err := tx.Exec(ctx, `
					UPDATE job_runs
					SET approval_gate        = true,
					    approvers            = $2,
					    approval_description = $3,
					    awaiting_since       = NOW(),
					    status               = 'awaiting_approval'
					WHERE id = $1
				`, row.ID, approvers, job.Approval.Description); err != nil {
					return RunCreated{}, fmt.Errorf("store: mark approval gate %s: %w", job.Name, err)
				}
			}
			result.JobRuns = append(result.JobRuns, JobRunRef{
				ID:         fromPgUUID(row.ID),
				StageRunID: stageID,
				Name:       job.Name,
				MatrixKey:  key,
			})
		}
	}

	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", RunQueuedChannel, result.RunID.String()); err != nil {
		return RunCreated{}, fmt.Errorf("store: notify run_queued: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RunCreated{}, fmt.Errorf("store: create run: commit: %w", err)
	}
	return result, nil
}

// expandMatrix returns the cartesian product of a matrix spec. An empty matrix
// collapses to a single zero-value combo, meaning "one job, no matrix_key".
// Iteration order across keys is sorted so job_run.matrix_key is deterministic.
func expandMatrix(m map[string][]string) []map[string]string {
	if len(m) == 0 {
		return []map[string]string{nil}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	combos := []map[string]string{{}}
	for _, k := range keys {
		values := m[k]
		if len(values) == 0 {
			continue
		}
		next := make([]map[string]string, 0, len(combos)*len(values))
		for _, prev := range combos {
			for _, v := range values {
				clone := make(map[string]string, len(prev)+1)
				for pk, pv := range prev {
					clone[pk] = pv
				}
				clone[k] = v
				next = append(next, clone)
			}
		}
		combos = next
	}
	return combos
}

func matrixKey(combo map[string]string) string {
	if len(combo) == 0 {
		return ""
	}
	keys := make([]string, 0, len(combo))
	for k := range combo {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+combo[k])
	}
	return strings.Join(parts, ",")
}
