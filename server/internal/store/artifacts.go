package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrArtifactNotFound is what GetArtifactByStorageKey returns when the
// storage_key doesn't match any row — typically an agent confirming a
// key we never issued, or a key that's been swept.
var ErrArtifactNotFound = errors.New("store: artifact not found")

// Artifact is the domain-shaped row. Mirrors the DB columns 1:1 except
// storage_key is the opaque backend key (see internal/artifacts).
type Artifact struct {
	ID            uuid.UUID
	RunID         uuid.UUID
	JobRunID      uuid.UUID
	PipelineID    uuid.UUID
	ProjectID     uuid.UUID
	Path          string
	StorageKey    string
	Status        string
	SizeBytes     int64
	ContentSHA256 string
	ExpiresAt     *time.Time
	PinnedAt      *time.Time
	DeletedAt     *time.Time
	CreatedAt     time.Time
}

// InsertPendingArtifact is what the gRPC RequestArtifactUpload handler
// calls once per declared path, *before* returning the signed PUT URL.
// Row lands in `pending`; agent upload + server HEAD flip it to `ready`.
type InsertPendingArtifact struct {
	RunID      uuid.UUID
	JobRunID   uuid.UUID
	PipelineID uuid.UUID
	ProjectID  uuid.UUID
	Path       string
	StorageKey string
	ExpiresAt  *time.Time // nil = never (pinned by default, or global default on the caller)
}

// InsertPendingArtifact creates the row.
func (s *Store) InsertPendingArtifact(ctx context.Context, in InsertPendingArtifact) (Artifact, error) {
	row, err := s.q.InsertPendingArtifact(ctx, db.InsertPendingArtifactParams{
		RunID:      pgUUID(in.RunID),
		JobRunID:   pgUUID(in.JobRunID),
		PipelineID: pgUUID(in.PipelineID),
		ProjectID:  pgUUID(in.ProjectID),
		Path:       in.Path,
		StorageKey: in.StorageKey,
		ExpiresAt:  pgTimestamptzFromPtr(in.ExpiresAt),
	})
	if err != nil {
		return Artifact{}, fmt.Errorf("store: insert artifact: %w", err)
	}
	return Artifact{
		ID:         fromPgUUID(row.ID),
		RunID:      fromPgUUID(row.RunID),
		JobRunID:   fromPgUUID(row.JobRunID),
		PipelineID: fromPgUUID(row.PipelineID),
		ProjectID:  fromPgUUID(row.ProjectID),
		Path:       row.Path,
		StorageKey: row.StorageKey,
		Status:     row.Status,
		ExpiresAt:  pgTimePtr(row.ExpiresAt),
		CreatedAt:  row.CreatedAt.Time,
	}, nil
}

// MarkArtifactReady flips a pending row to ready once the server
// confirmed the object via Store.Head. Returns whether the row was
// updated (false = already ready, or swept, or never existed). Callers
// decide whether a non-update is an error — for the JobResult path we
// just log and continue.
func (s *Store) MarkArtifactReady(ctx context.Context, storageKey string, size int64, sha256 string) (bool, error) {
	n, err := s.q.MarkArtifactReady(ctx, db.MarkArtifactReadyParams{
		StorageKey:    storageKey,
		SizeBytes:     size,
		ContentSha256: sha256,
	})
	if err != nil {
		return false, fmt.Errorf("store: mark artifact ready: %w", err)
	}
	return n > 0, nil
}

// GetArtifactByStorageKey returns ErrArtifactNotFound if the key is
// unknown. Used only on the JobResult reconciliation path today.
func (s *Store) GetArtifactByStorageKey(ctx context.Context, storageKey string) (Artifact, error) {
	row, err := s.q.GetArtifactByStorageKey(ctx, storageKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, ErrArtifactNotFound
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("store: get artifact: %w", err)
	}
	return artifactFromGetRow(row), nil
}

// ListArtifactsByJobRun returns every artifact row for a job_run
// (regardless of status). Used by the JobResult handler to cross-check
// what the agent reported against what we issued PUT URLs for.
func (s *Store) ListArtifactsByJobRun(ctx context.Context, jobRunID uuid.UUID) ([]Artifact, error) {
	rows, err := s.q.ListArtifactsByJobRun(ctx, pgUUID(jobRunID))
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts by job_run: %w", err)
	}
	out := make([]Artifact, 0, len(rows))
	for _, r := range rows {
		out = append(out, artifactFromListJobRunRow(r))
	}
	return out, nil
}

// ListReadyArtifactsByRun is the E2c download-resolution path. Only
// ready + not-deleted rows — pending artefacts must not be surfaced as
// something downstream jobs can depend on.
func (s *Store) ListReadyArtifactsByRun(ctx context.Context, runID uuid.UUID) ([]Artifact, error) {
	rows, err := s.q.ListArtifactsByRun(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts by run: %w", err)
	}
	out := make([]Artifact, 0, len(rows))
	for _, r := range rows {
		out = append(out, artifactFromListRunRow(r))
	}
	return out, nil
}

// ArtifactWithJob is Artifact + job_runs.name, the shape the runs API
// returns so the UI can group by job without per-row lookups.
type ArtifactWithJob struct {
	Artifact
	JobName string
}

// ListArtifactsWithJobByRun is the UI-facing read path. Returns every
// non-deleted row (any status) so the UI can show "pending" rows with a
// spinner instead of hiding them.
func (s *Store) ListArtifactsWithJobByRun(ctx context.Context, runID uuid.UUID) ([]ArtifactWithJob, error) {
	rows, err := s.q.ListArtifactsWithJobByRun(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts w/ job by run: %w", err)
	}
	out := make([]ArtifactWithJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, ArtifactWithJob{
			Artifact: Artifact{
				ID:            fromPgUUID(r.ID),
				RunID:         fromPgUUID(r.RunID),
				JobRunID:      fromPgUUID(r.JobRunID),
				PipelineID:    fromPgUUID(r.PipelineID),
				ProjectID:     fromPgUUID(r.ProjectID),
				Path:          r.Path,
				StorageKey:    r.StorageKey,
				Status:        r.Status,
				SizeBytes:     r.SizeBytes,
				ContentSHA256: r.ContentSha256,
				ExpiresAt:     pgTimePtr(r.ExpiresAt),
				PinnedAt:      pgTimePtr(r.PinnedAt),
				DeletedAt:     pgTimePtr(r.DeletedAt),
				CreatedAt:     r.CreatedAt.Time,
			},
			JobName: r.JobName,
		})
	}
	return out, nil
}

// JobRunParents returns pipeline_id + project_id + dispatched agent_id
// for a (job_run_id, run_id) pair, and ErrArtifactNotFound if the job
// doesn't belong to the claimed run. agent_id is uuid.Nil if the job
// hasn't been dispatched yet. Used for authz + FK derivation in the
// artifact upload path.
func (s *Store) JobRunParents(ctx context.Context, jobRunID, runID uuid.UUID) (pipelineID, projectID, agentID uuid.UUID, err error) {
	row, err := s.q.GetJobRunParents(ctx, db.GetJobRunParentsParams{
		ID:    pgUUID(jobRunID),
		RunID: pgUUID(runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, uuid.Nil, ErrArtifactNotFound
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("store: job run parents: %w", err)
	}
	return fromPgUUID(row.PipelineID), fromPgUUID(row.ProjectID), fromPgUUID(row.AgentID), nil
}

// --- conversions ---

func pgTimestamptzFromPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func artifactFromGetRow(r db.GetArtifactByStorageKeyRow) Artifact {
	return Artifact{
		ID:            fromPgUUID(r.ID),
		RunID:         fromPgUUID(r.RunID),
		JobRunID:      fromPgUUID(r.JobRunID),
		PipelineID:    fromPgUUID(r.PipelineID),
		ProjectID:     fromPgUUID(r.ProjectID),
		Path:          r.Path,
		StorageKey:    r.StorageKey,
		Status:        r.Status,
		SizeBytes:     r.SizeBytes,
		ContentSHA256: r.ContentSha256,
		ExpiresAt:     pgTimePtr(r.ExpiresAt),
		PinnedAt:      pgTimePtr(r.PinnedAt),
		DeletedAt:     pgTimePtr(r.DeletedAt),
		CreatedAt:     r.CreatedAt.Time,
	}
}

func artifactFromListJobRunRow(r db.ListArtifactsByJobRunRow) Artifact {
	return Artifact{
		ID:            fromPgUUID(r.ID),
		RunID:         fromPgUUID(r.RunID),
		JobRunID:      fromPgUUID(r.JobRunID),
		PipelineID:    fromPgUUID(r.PipelineID),
		ProjectID:     fromPgUUID(r.ProjectID),
		Path:          r.Path,
		StorageKey:    r.StorageKey,
		Status:        r.Status,
		SizeBytes:     r.SizeBytes,
		ContentSHA256: r.ContentSha256,
		ExpiresAt:     pgTimePtr(r.ExpiresAt),
		PinnedAt:      pgTimePtr(r.PinnedAt),
		DeletedAt:     pgTimePtr(r.DeletedAt),
		CreatedAt:     r.CreatedAt.Time,
	}
}

func artifactFromListRunRow(r db.ListArtifactsByRunRow) Artifact {
	return Artifact{
		ID:            fromPgUUID(r.ID),
		RunID:         fromPgUUID(r.RunID),
		JobRunID:      fromPgUUID(r.JobRunID),
		PipelineID:    fromPgUUID(r.PipelineID),
		ProjectID:     fromPgUUID(r.ProjectID),
		Path:          r.Path,
		StorageKey:    r.StorageKey,
		Status:        r.Status,
		SizeBytes:     r.SizeBytes,
		ContentSHA256: r.ContentSha256,
		ExpiresAt:     pgTimePtr(r.ExpiresAt),
		PinnedAt:      pgTimePtr(r.PinnedAt),
		DeletedAt:     pgTimePtr(r.DeletedAt),
		CreatedAt:     r.CreatedAt.Time,
	}
}
