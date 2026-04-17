package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// CreateRunFromUpstreamInput captures the upstream context needed to spawn a
// downstream run triggered by a stage-passed event (the GoCD fan-out pattern).
type CreateRunFromUpstreamInput struct {
	DownstreamPipelineID uuid.UUID
	DownstreamMaterialID uuid.UUID

	UpstreamRunID        uuid.UUID
	UpstreamRunCounter   int64
	UpstreamPipelineName string
	UpstreamStageName    string

	// UpstreamRevisions is the raw JSONB from runs.revisions. Shallow-merged
	// into the downstream run so shared materials propagate the same commit —
	// when a real checkout happens on the downstream, the scheduler's
	// assignment builder picks whichever entry matches the material UUID.
	UpstreamRevisions json.RawMessage
}

// CreateRunFromUpstream is idempotent: calling it twice for the same
// (downstream pipeline, upstream run) returns the already-created run without
// a second insert. That makes fan-out safe under retry.
func (s *Store) CreateRunFromUpstream(ctx context.Context, in CreateRunFromUpstreamInput) (RunCreated, bool, error) {
	existing, err := s.q.FindRunByUpstream(ctx, db.FindRunByUpstreamParams{
		PipelineID:    pgUUID(in.DownstreamPipelineID),
		UpstreamRunID: pgUUID(in.UpstreamRunID),
	})
	if err == nil {
		return RunCreated{RunID: fromPgUUID(existing.ID), Counter: existing.Counter}, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RunCreated{}, false, fmt.Errorf("store: upstream dedup: %w", err)
	}

	causeDetail, _ := json.Marshal(map[string]any{
		"upstream_run_id":      in.UpstreamRunID.String(),
		"upstream_run_counter": in.UpstreamRunCounter,
		"upstream_pipeline":    in.UpstreamPipelineName,
		"upstream_stage":       in.UpstreamStageName,
	})

	revs := map[string]any{}
	if len(in.UpstreamRevisions) > 0 {
		_ = json.Unmarshal(in.UpstreamRevisions, &revs)
	}
	// Stamp this specific upstream material so a single run can track multiple
	// upstream parents — useful once multi-upstream is wired later.
	revs[in.DownstreamMaterialID.String()] = map[string]string{
		"revision": in.UpstreamRunID.String(),
		"branch":   "",
	}
	revisions, _ := json.Marshal(revs)

	created, err := s.insertRunSkeleton(ctx, insertRunSkeletonInput{
		PipelineID:  in.DownstreamPipelineID,
		Cause:       string(domain.CauseUpstream),
		CauseDetail: causeDetail,
		Revisions:   revisions,
		TriggeredBy: "system:upstream",
	})
	if err != nil {
		return RunCreated{}, false, err
	}
	return created, true, nil
}

// FanoutTriggeredRun is the per-downstream outcome returned from FanoutFromStage.
type FanoutTriggeredRun struct {
	DownstreamPipelineID uuid.UUID
	DownstreamMaterialID uuid.UUID
	Run                  RunCreated
	Created              bool // false if FindRunByUpstream already had a row
}

// FanoutFromStage finds every downstream pipeline whose `upstream` material
// matches this stage + pipeline and queues a run for each. Partial failures
// are reported via the returned error (joined) so one bad downstream doesn't
// swallow siblings.
func (s *Store) FanoutFromStage(ctx context.Context, stageRunID uuid.UUID) ([]FanoutTriggeredRun, error) {
	summary, err := s.q.GetStageSummary(ctx, pgUUID(stageRunID))
	if err != nil {
		return nil, fmt.Errorf("store: fanout: stage summary: %w", err)
	}

	matches, err := s.q.FindDownstreamUpstreamMaterials(ctx, db.FindDownstreamUpstreamMaterialsParams{
		UpstreamPipelineID: summary.PipelineID,
		StageName:          summary.StageName,
	})
	if err != nil {
		return nil, fmt.Errorf("store: fanout: list downstreams: %w", err)
	}

	var errs []error
	out := make([]FanoutTriggeredRun, 0, len(matches))
	for _, m := range matches {
		rc, created, err := s.CreateRunFromUpstream(ctx, CreateRunFromUpstreamInput{
			DownstreamPipelineID: fromPgUUID(m.DownstreamPipelineID),
			DownstreamMaterialID: fromPgUUID(m.MaterialID),
			UpstreamRunID:        fromPgUUID(summary.RunID),
			UpstreamRunCounter:   summary.Counter,
			UpstreamPipelineName: summary.PipelineName,
			UpstreamStageName:    summary.StageName,
			UpstreamRevisions:    summary.Revisions,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("downstream pipeline %s: %w", fromPgUUID(m.DownstreamPipelineID), err))
			continue
		}
		out = append(out, FanoutTriggeredRun{
			DownstreamPipelineID: fromPgUUID(m.DownstreamPipelineID),
			DownstreamMaterialID: fromPgUUID(m.MaterialID),
			Run:                  rc,
			Created:              created,
		})
	}
	return out, errors.Join(errs...)
}
