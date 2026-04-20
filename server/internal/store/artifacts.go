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

// ListReadyArtifactsByRunAndJob returns only the ready artifacts a
// given job name produced within a run. Optional paths filter narrows
// further — empty = all that job's artefacts. Used by the scheduler
// when resolving `needs_artifacts` for a downstream job.
func (s *Store) ListReadyArtifactsByRunAndJob(ctx context.Context, runID uuid.UUID, jobName string, paths []string) ([]ArtifactWithJob, error) {
	if paths == nil {
		paths = []string{}
	}
	rows, err := s.q.ListReadyArtifactsByRunAndJobName(ctx, db.ListReadyArtifactsByRunAndJobNameParams{
		RunID:   pgUUID(runID),
		Name:    jobName,
		Column3: paths,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list ready artifacts by job: %w", err)
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

// ClaimedArtifact is what the sweeper iterates over: enough to call
// Store.Delete + RemoveArtifactRow. SizeBytes flows through so
// observability can report bytes freed per tick.
type ClaimedArtifact struct {
	ID         uuid.UUID
	StorageKey string
	SizeBytes  int64
}

// ExpireArtifactsBeyondKeepLast stamps expires_at=NOW() on artefacts
// in runs ranked beyond the N most recent per pipeline. Returns the
// number of rows demoted. The actual delete happens on a following
// tick via the TTL path — keeping the delete flow in one place.
func (s *Store) ExpireArtifactsBeyondKeepLast(ctx context.Context, keepLast int) (int64, error) {
	n, err := s.q.ExpireArtifactsBeyondKeepLast(ctx, int32(keepLast))
	if err != nil {
		return 0, fmt.Errorf("store: keep-last expire: %w", err)
	}
	return n, nil
}

// ProjectOverQuota is one entry returned by ListProjectsOverArtifactQuota.
type ProjectOverQuota struct {
	ProjectID uuid.UUID
	Bytes     int64
}

// ListProjectsOverArtifactQuota returns projects whose live bytes
// exceed `quota`. Used by the sweeper to iterate overquota projects
// once per tick.
func (s *Store) ListProjectsOverArtifactQuota(ctx context.Context, quota int64) ([]ProjectOverQuota, error) {
	rows, err := s.q.ListProjectsOverArtifactQuota(ctx, quota)
	if err != nil {
		return nil, fmt.Errorf("store: list over-quota projects: %w", err)
	}
	out := make([]ProjectOverQuota, 0, len(rows))
	for _, r := range rows {
		out = append(out, ProjectOverQuota{
			ProjectID: fromPgUUID(r.ProjectID),
			Bytes:     r.Bytes,
		})
	}
	return out, nil
}

// ExpireOldestInProjectByExcess demotes the oldest non-pinned rows in
// a project until cumulative demoted bytes cover `excess`. Returns
// rows affected.
func (s *Store) ExpireOldestInProjectByExcess(ctx context.Context, projectID uuid.UUID, excess int64) (int64, error) {
	n, err := s.q.ExpireOldestInProjectByExcess(ctx, db.ExpireOldestInProjectByExcessParams{
		Pid:    pgUUID(projectID),
		Excess: excess,
	})
	if err != nil {
		return 0, fmt.Errorf("store: project-quota expire: %w", err)
	}
	return n, nil
}

// GlobalArtifactUsage sums non-deleted + non-pinned live bytes across
// all projects. Used for the global hard cap.
func (s *Store) GlobalArtifactUsage(ctx context.Context) (int64, error) {
	n, err := s.q.GlobalArtifactUsage(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: global usage: %w", err)
	}
	return n, nil
}

// ExpireOldestGloballyByExcess is the global-cap variant: demotes the
// oldest non-pinned rows across the whole system.
func (s *Store) ExpireOldestGloballyByExcess(ctx context.Context, excess int64) (int64, error) {
	n, err := s.q.ExpireOldestGloballyByExcess(ctx, excess)
	if err != nil {
		return 0, fmt.Errorf("store: global-quota expire: %w", err)
	}
	return n, nil
}

// ClaimArtifactsForSweep atomically marks a batch of expired / stale-
// deleting artefacts for removal, returning their storage keys. Caller
// deletes each from the backend, then calls RemoveArtifactRow. grace
// is the window after which a row stuck in 'deleting' is eligible for
// retry (typical 5 minutes). limit caps the batch.
func (s *Store) ClaimArtifactsForSweep(ctx context.Context, limit int, graceMinutes int) ([]ClaimedArtifact, error) {
	rows, err := s.q.ClaimArtifactsForSweep(ctx, db.ClaimArtifactsForSweepParams{
		Limit:   int32(limit),
		Column2: int32(graceMinutes),
	})
	if err != nil {
		return nil, fmt.Errorf("store: claim artifacts: %w", err)
	}
	out := make([]ClaimedArtifact, 0, len(rows))
	for _, r := range rows {
		out = append(out, ClaimedArtifact{
			ID:         fromPgUUID(r.ID),
			StorageKey: r.StorageKey,
			SizeBytes:  r.SizeBytes,
		})
	}
	return out, nil
}

// RemoveArtifactRow deletes the DB row after the sweeper confirmed the
// storage object is gone. Separate from the storage delete so we can
// retry independently.
func (s *Store) RemoveArtifactRow(ctx context.Context, id uuid.UUID) error {
	if _, err := s.q.RemoveArtifactRow(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: remove artifact row: %w", err)
	}
	return nil
}

// RunUpstreamContext is what the scheduler needs to resolve a
// cross-run `needs_artifacts` entry: which run produced the artifacts
// (because fanout downstream runs carry the upstream's id in
// cause_detail) and which pipeline name that run belongs to.
// UpstreamRunID is uuid.Nil when the current run is not a fanout
// downstream (webhook/manual). In that case UpstreamPipeline is "".
type RunUpstreamContext struct {
	UpstreamRunID    uuid.UUID
	UpstreamPipeline string
}

// GetRunUpstreamContext returns the upstream-run context from a
// run's cause_detail. Returns zeros when no upstream is set.
func (s *Store) GetRunUpstreamContext(ctx context.Context, runID uuid.UUID) (RunUpstreamContext, error) {
	row, err := s.q.GetRunUpstreamContext(ctx, pgUUID(runID))
	if err != nil {
		return RunUpstreamContext{}, fmt.Errorf("store: upstream context: %w", err)
	}
	return RunUpstreamContext{
		UpstreamRunID:    fromPgUUID(row.UpstreamRunID),
		UpstreamPipeline: row.UpstreamPipeline,
	}, nil
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
