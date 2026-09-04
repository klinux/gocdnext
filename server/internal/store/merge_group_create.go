package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

var (
	ErrMergeGroupDestroyed    = errors.New("store: merge group already destroyed")
	ErrMergeGroupInvalidInput = errors.New("store: merge group: invalid input")
)

const mergeGroupLockNamespace int32 = 0x4D4751 // "MGQ"

type MergeGroupRunInput struct {
	Fingerprint string
	PipelineID  uuid.UUID
	MaterialID  uuid.UUID
	Revision    string
	Branch      string
	Author      string
	Message     string
	Payload     json.RawMessage
	CommittedAt time.Time
	Provider    string
	Delivery    string
	TriggeredBy string
	CauseDetail json.RawMessage
}

type MergeGroupRunResult struct {
	Run                 RunCreated
	ModificationID      int64
	ModificationCreated bool
	RunCreated          bool
}

// CreateOrFindMergeGroupRun inserts the merge-group modification and its run in
// one transaction. A replay returns the existing run for the same
// material/revision/branch so the caller can re-post the pending check instead
// of silently skipping it.
func (s *Store) CreateOrFindMergeGroupRun(ctx context.Context, in MergeGroupRunInput) (MergeGroupRunResult, error) {
	in.Fingerprint = strings.TrimSpace(in.Fingerprint)
	if in.Fingerprint == "" || in.PipelineID == uuid.Nil || in.MaterialID == uuid.Nil ||
		strings.TrimSpace(in.Revision) == "" || strings.TrimSpace(in.Branch) == "" ||
		in.Provider == "" || in.Delivery == "" {
		return MergeGroupRunResult{}, ErrMergeGroupInvalidInput
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MergeGroupRunResult{}, fmt.Errorf("store: merge group create run: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := lockMergeGroupTx(ctx, tx, in.Fingerprint, in.Revision); err != nil {
		return MergeGroupRunResult{}, fmt.Errorf("store: merge group create run: lock: %w", err)
	}
	destroyed, err := mergeGroupDestroyedQ(ctx, tx, in.Fingerprint, in.Revision)
	if err != nil {
		return MergeGroupRunResult{}, fmt.Errorf("store: merge group create run: destroyed check: %w", err)
	}
	if destroyed {
		return MergeGroupRunResult{}, ErrMergeGroupDestroyed
	}

	modRes, err := insertModificationQ(ctx, q, Modification{
		MaterialID:  in.MaterialID,
		Revision:    in.Revision,
		Branch:      in.Branch,
		Author:      in.Author,
		Message:     in.Message,
		Payload:     in.Payload,
		CommittedAt: in.CommittedAt,
	})
	if err != nil {
		return MergeGroupRunResult{}, err
	}

	if !modRes.Created {
		existing, ok, err := findMergeGroupRunByModificationQ(ctx, tx, in.PipelineID, modRes.ID)
		if err != nil {
			return MergeGroupRunResult{}, fmt.Errorf("store: merge group create run: lookup existing run: %w", err)
		}
		if ok {
			if err := tx.Commit(ctx); err != nil {
				return MergeGroupRunResult{}, fmt.Errorf("store: merge group create run: commit replay: %w", err)
			}
			return MergeGroupRunResult{
				Run:                 existing,
				ModificationID:      modRes.ID,
				ModificationCreated: false,
				RunCreated:          false,
			}, nil
		}
	}

	pipelineRow, err := q.GetPipelineDefinition(ctx, pgUUID(in.PipelineID))
	if err != nil {
		return MergeGroupRunResult{}, fmt.Errorf("store: merge group create run: load pipeline %s: %w", in.PipelineID, err)
	}
	runDef, err := effectiveDefFromBytes(pipelineRow.Definition)
	if err != nil {
		return MergeGroupRunResult{}, fmt.Errorf("store: merge group create run: %w", err)
	}
	if len(runDef.pipeline.Stages) == 0 {
		return MergeGroupRunResult{}, fmt.Errorf("store: merge group create run: pipeline %s has no stages", in.PipelineID)
	}
	causeDetail, err := mergeGroupCauseDetail(in, modRes.ID)
	if err != nil {
		return MergeGroupRunResult{}, err
	}
	revisions, _ := json.Marshal(map[string]any{
		in.MaterialID.String(): map[string]string{
			"revision": in.Revision,
			"branch":   in.Branch,
		},
	})

	var pendingAuditEmits []AuditEmit
	run, err := s.insertRunRowsTx(ctx, tx, q, runRowsSpec{
		PipelineID:    in.PipelineID,
		Def:           runDef,
		ProjectNotifs: pipelineRow.ProjectNotifications,
		Cause:         string(domain.CauseMergeGroup),
		CauseDetail:   causeDetail,
		Revisions:     revisions,
		TriggeredBy:   in.TriggeredBy,
		Ref:           in.Branch,
	}, &pendingAuditEmits)
	if err != nil {
		return MergeGroupRunResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return MergeGroupRunResult{}, fmt.Errorf("store: merge group create run: commit: %w", err)
	}
	for _, emit := range pendingAuditEmits {
		if _, err := s.EmitAuditEvent(ctx, emit); err != nil {
			slog.Warn("store: merge group create run: audit emit failed",
				"err", err, "target_id", emit.TargetID, "action", emit.Action)
		}
	}
	return MergeGroupRunResult{
		Run:                 run,
		ModificationID:      modRes.ID,
		ModificationCreated: modRes.Created,
		RunCreated:          true,
	}, nil
}

func mergeGroupCauseDetail(in MergeGroupRunInput, modID int64) (json.RawMessage, error) {
	detail := map[string]any{}
	if len(in.CauseDetail) > 0 {
		if err := json.Unmarshal(in.CauseDetail, &detail); err != nil {
			return nil, fmt.Errorf("%w: cause_detail must be a json object: %v", ErrMergeGroupInvalidInput, err)
		}
		if detail == nil {
			return nil, fmt.Errorf("%w: cause_detail must be a non-null json object", ErrMergeGroupInvalidInput)
		}
	}
	detail["provider"] = in.Provider
	detail["delivery"] = in.Delivery
	detail["material_id"] = in.MaterialID.String()
	detail["modification_id"] = modID
	detail["mg_fingerprint"] = in.Fingerprint
	out, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func findMergeGroupRunByModificationQ(ctx context.Context, tx pgx.Tx, pipelineID uuid.UUID, modificationID int64) (RunCreated, bool, error) {
	var runID uuid.UUID
	var counter int64
	err := tx.QueryRow(ctx, `
		SELECT id, counter
		FROM runs
		WHERE pipeline_id = $1
		  AND cause = $2
		  AND cause_detail->>'modification_id' = $3
		ORDER BY created_at ASC
		LIMIT 1
	`, pgUUID(pipelineID), string(domain.CauseMergeGroup), strconv.FormatInt(modificationID, 10)).Scan(&runID, &counter)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunCreated{}, false, nil
	}
	if err != nil {
		return RunCreated{}, false, err
	}
	return RunCreated{RunID: runID, Counter: counter}, true, nil
}

func lockMergeGroupTx(ctx context.Context, tx pgx.Tx, fingerprint, headSHA string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int, hashtext($2))`,
		mergeGroupLockNamespace, fingerprint+"::"+headSHA)
	return err
}

type mergeGroupRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func mergeGroupDestroyedQ(ctx context.Context, q mergeGroupRowQuerier, fingerprint, headSHA string) (bool, error) {
	var destroyed bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM merge_group_destroyed
			WHERE fingerprint = $1 AND head_sha = $2
		)
	`, fingerprint, headSHA).Scan(&destroyed)
	return destroyed, err
}

// MergeGroupDestroyed reports whether a destroyed tombstone already exists for
// the queue entry. The webhook path uses this to drop stale checks_requested
// deliveries that arrive after destroyed.
func (s *Store) MergeGroupDestroyed(ctx context.Context, fingerprint, headSHA string) (bool, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	headSHA = strings.TrimSpace(headSHA)
	if fingerprint == "" || headSHA == "" {
		return false, ErrMergeGroupInvalidInput
	}
	return mergeGroupDestroyedQ(ctx, s.pool, fingerprint, headSHA)
}
