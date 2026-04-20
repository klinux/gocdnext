package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrAgentByIDNotFound signals FindAgentWithRunning saw no row.
var ErrAgentByIDNotFound = errors.New("store: agent not found")

// DashboardMetrics is the payload the `/` widget row needs.
type DashboardMetrics struct {
	RunsToday      int64   `json:"runs_today"`
	Successes7d    int64   `json:"successes_7d"`
	Failures7d     int64   `json:"failures_7d"`
	Canceled7d     int64   `json:"canceled_7d"`
	SuccessRate7d  float64 `json:"success_rate_7d"` // 0..1 over (success+failure)
	P50Seconds7d   float64 `json:"p50_seconds_7d"`
	QueuedRuns     int64   `json:"queued_runs"`
	PendingJobs    int64   `json:"pending_jobs"`
}

// GetDashboardMetrics issues the four small queries in parallel-
// safe sequence (pgx pool handles the concurrency just fine) and
// composes the payload. Returns zero values when the DB has no
// runs yet — dashboard shows "0" in every tile rather than an
// error state.
func (s *Store) GetDashboardMetrics(ctx context.Context) (DashboardMetrics, error) {
	today, err := s.q.DashboardRunsToday(ctx)
	if err != nil {
		return DashboardMetrics{}, fmt.Errorf("store: runs today: %w", err)
	}
	rates, err := s.q.DashboardSuccessRate7d(ctx)
	if err != nil {
		return DashboardMetrics{}, fmt.Errorf("store: success rate: %w", err)
	}
	p50, err := s.q.DashboardP50DurationSec7d(ctx)
	if err != nil {
		return DashboardMetrics{}, fmt.Errorf("store: p50 duration: %w", err)
	}
	queue, err := s.q.DashboardQueueDepth(ctx)
	if err != nil {
		return DashboardMetrics{}, fmt.Errorf("store: queue depth: %w", err)
	}

	var successRate float64
	if denom := rates.Successes + rates.Failures; denom > 0 {
		successRate = float64(rates.Successes) / float64(denom)
	}

	return DashboardMetrics{
		RunsToday:     today,
		Successes7d:   rates.Successes,
		Failures7d:    rates.Failures,
		Canceled7d:    rates.Canceled,
		SuccessRate7d: successRate,
		P50Seconds7d:  p50,
		QueuedRuns:    queue.QueuedRuns,
		PendingJobs:   queue.PendingJobs,
	}, nil
}

// GlobalRunSummary is the row shape returned by ListRunsGlobal; it
// extends RunSummary with a project reference so the UI can link
// straight to the owning project (avoids a second query per row).
type GlobalRunSummary struct {
	RunSummary
	ProjectID   uuid.UUID `json:"project_id"`
	ProjectSlug string    `json:"project_slug"`
	ProjectName string    `json:"project_name"`
}

// RunsFilter bundles the optional filters for ListRunsGlobal /
// CountRunsGlobal. Empty strings mean "no filter" on that axis.
type RunsFilter struct {
	Status      string
	Cause       string
	ProjectSlug string
}

// ListRunsGlobal returns a slice of GlobalRunSummary matching the
// filter, paged by limit/offset. Offset defaults to 0 when negative.
// Used by both the dashboard widget (filter empty, limit=20) and
// the /runs page (full filter surface, paginated).
func (s *Store) ListRunsGlobal(ctx context.Context, limit int32, offset int64, filter RunsFilter) ([]GlobalRunSummary, error) {
	if offset < 0 {
		offset = 0
	}
	rows, err := s.q.ListRunsGlobal(ctx, db.ListRunsGlobalParams{
		Limit:        limit,
		StatusFilter: filter.Status,
		CauseFilter:  filter.Cause,
		ProjectSlug:  filter.ProjectSlug,
		RowOffset:    offset,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list runs global: %w", err)
	}
	out := make([]GlobalRunSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, GlobalRunSummary{
			RunSummary: RunSummary{
				ID:           fromPgUUID(r.ID),
				PipelineID:   fromPgUUID(r.PipelineID),
				PipelineName: r.PipelineName,
				Counter:      r.Counter,
				Cause:        r.Cause,
				Status:       r.Status,
				CreatedAt:    r.CreatedAt.Time,
				StartedAt:    pgTimePtr(r.StartedAt),
				FinishedAt:   pgTimePtr(r.FinishedAt),
				TriggeredBy:  stringValue(r.TriggeredBy),
			},
			ProjectID:   fromPgUUID(r.ProjectID),
			ProjectSlug: r.ProjectSlug,
			ProjectName: r.ProjectName,
		})
	}
	return out, nil
}

// CountRunsGlobal returns the total matching the same filter, for
// paged "N of M" displays.
func (s *Store) CountRunsGlobal(ctx context.Context, filter RunsFilter) (int64, error) {
	n, err := s.q.CountRunsGlobal(ctx, db.CountRunsGlobalParams{
		StatusFilter: filter.Status,
		CauseFilter:  filter.Cause,
		ProjectSlug:  filter.ProjectSlug,
	})
	if err != nil {
		return 0, fmt.Errorf("store: count runs global: %w", err)
	}
	return n, nil
}

// AgentSummary is what dashboards + the /agents page show. Derived
// status (online/offline/idle) is computed in Go instead of SQL —
// lets UI tune the "offline if last_seen > N minutes ago" threshold
// without a migration.
type AgentSummary struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	Version      string     `json:"version,omitempty"`
	OS           string     `json:"os,omitempty"`
	Arch         string     `json:"arch,omitempty"`
	Tags         []string   `json:"tags"`
	Capacity     int32      `json:"capacity"`
	Status       string     `json:"status"` // raw value from agents.status
	HealthState  string     `json:"health_state"` // online | stale | offline
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
	RegisteredAt time.Time  `json:"registered_at"`
	RunningJobs  int64      `json:"running_jobs"`
}

// AgentStaleAfter is the window after which an "online" agent
// stops looking live on the dashboard. Picked conservatively so a
// stuck heartbeat loop is visible within ~2 heartbeats at the
// server's 30s default.
const AgentStaleAfter = 90 * time.Second

// FindAgentWithRunning returns the single-agent shape used by the
// /agents/{id} page. Returns ErrAgentByIDNotFound when the UUID
// doesn't exist — handler maps that to 404.
func (s *Store) FindAgentWithRunning(ctx context.Context, id uuid.UUID, now time.Time) (AgentSummary, error) {
	row, err := s.q.FindAgentWithRunning(ctx, pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentSummary{}, ErrAgentByIDNotFound
	}
	if err != nil {
		return AgentSummary{}, fmt.Errorf("store: find agent: %w", err)
	}
	lastSeen := pgTimePtr(row.LastSeenAt)
	return AgentSummary{
		ID:           fromPgUUID(row.ID),
		Name:         row.Name,
		Version:      stringValue(row.Version),
		OS:           stringValue(row.Os),
		Arch:         stringValue(row.Arch),
		Tags:         append([]string(nil), row.Tags...),
		Capacity:     row.Capacity,
		Status:       row.Status,
		HealthState:  deriveHealth(row.Status, lastSeen, now),
		LastSeenAt:   lastSeen,
		RegisteredAt: row.RegisteredAt.Time,
		RunningJobs:  row.RunningJobs,
	}, nil
}

// AgentJobSummary is one row in the /agents/{id} recent-jobs table.
type AgentJobSummary struct {
	JobRunID     uuid.UUID  `json:"job_run_id"`
	JobName      string     `json:"job_name"`
	JobStatus    string     `json:"job_status"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	ExitCode     *int32     `json:"exit_code,omitempty"`
	RunID        uuid.UUID  `json:"run_id"`
	RunCounter   int64      `json:"run_counter"`
	PipelineName string     `json:"pipeline_name"`
	ProjectID    uuid.UUID  `json:"project_id"`
	ProjectSlug  string     `json:"project_slug"`
	ProjectName  string     `json:"project_name"`
}

// ListJobsForAgent returns the N most recent jobs dispatched to
// this agent, newest first. Joins up to the owning project so the
// UI table links directly.
func (s *Store) ListJobsForAgent(ctx context.Context, id uuid.UUID, limit int32) ([]AgentJobSummary, error) {
	rows, err := s.q.ListJobsForAgent(ctx, db.ListJobsForAgentParams{
		AgentID: pgUUID(id),
		Limit:   limit,
	})
	if err != nil {
		return nil, fmt.Errorf("store: agent jobs: %w", err)
	}
	out := make([]AgentJobSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, AgentJobSummary{
			JobRunID:     fromPgUUID(r.JobRunID),
			JobName:      r.JobName,
			JobStatus:    r.JobStatus,
			StartedAt:    pgTimePtr(r.StartedAt),
			FinishedAt:   pgTimePtr(r.FinishedAt),
			ExitCode:     r.ExitCode,
			RunID:        fromPgUUID(r.RunID),
			RunCounter:   r.RunCounter,
			PipelineName: r.PipelineName,
			ProjectID:    fromPgUUID(r.ProjectID),
			ProjectSlug:  r.ProjectSlug,
			ProjectName:  r.ProjectName,
		})
	}
	return out, nil
}

// deriveHealth collapses the agent's raw status + last-seen age
// into the UI-visible state. Factored out so FindAgent and
// ListAgents share the same threshold (AgentStaleAfter).
func deriveHealth(status string, lastSeen *time.Time, now time.Time) string {
	switch status {
	case "online":
		if lastSeen != nil && now.Sub(*lastSeen) <= AgentStaleAfter {
			return "online"
		}
		return "stale"
	case "idle":
		return "idle"
	}
	return "offline"
}

// ListAgentsWithRunning returns every agent plus its current
// running+queued job count. Handler decorates with HealthState
// derived from LastSeenAt.
func (s *Store) ListAgentsWithRunning(ctx context.Context, now time.Time) ([]AgentSummary, error) {
	rows, err := s.q.ListAgentsWithRunning(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list agents: %w", err)
	}
	out := make([]AgentSummary, 0, len(rows))
	for _, r := range rows {
		lastSeen := pgTimePtr(r.LastSeenAt)
		out = append(out, AgentSummary{
			ID:           fromPgUUID(r.ID),
			Name:         r.Name,
			Version:      stringValue(r.Version),
			OS:           stringValue(r.Os),
			Arch:         stringValue(r.Arch),
			Tags:         append([]string(nil), r.Tags...),
			Capacity:     r.Capacity,
			Status:       r.Status,
			HealthState:  deriveHealth(r.Status, lastSeen, now),
			LastSeenAt:   lastSeen,
			RegisteredAt: r.RegisteredAt.Time,
			RunningJobs:  r.RunningJobs,
		})
	}
	return out, nil
}
