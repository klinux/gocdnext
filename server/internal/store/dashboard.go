package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

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

// ListRunsGlobal returns the N most recent runs across all
// projects. Optional status filter narrows to a single status
// (empty = any).
func (s *Store) ListRunsGlobal(ctx context.Context, limit int32, statusFilter string) ([]GlobalRunSummary, error) {
	rows, err := s.q.ListRunsGlobal(ctx, db.ListRunsGlobalParams{
		Limit:        limit,
		StatusFilter: statusFilter,
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
		health := "offline"
		switch r.Status {
		case "online":
			if lastSeen != nil && now.Sub(*lastSeen) <= AgentStaleAfter {
				health = "online"
			} else {
				health = "stale"
			}
		case "idle":
			health = "idle"
		}
		out = append(out, AgentSummary{
			ID:           fromPgUUID(r.ID),
			Name:         r.Name,
			Version:      stringValue(r.Version),
			OS:           stringValue(r.Os),
			Arch:         stringValue(r.Arch),
			Tags:         append([]string(nil), r.Tags...),
			Capacity:     r.Capacity,
			Status:       r.Status,
			HealthState:  health,
			LastSeenAt:   lastSeen,
			RegisteredAt: r.RegisteredAt.Time,
			RunningJobs:  r.RunningJobs,
		})
	}
	return out, nil
}
