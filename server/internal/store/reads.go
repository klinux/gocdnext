package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// Sentinel error for read paths that need to distinguish "no row" from real
// failure (HTTP handlers turn this into 404).
var ErrProjectNotFound = errors.New("store: project not found")
var ErrRunNotFound = errors.New("store: run not found")

// ProjectSummary is the dashboard-level view of a project.
type ProjectSummary struct {
	ID            uuid.UUID  `json:"id"`
	Slug          string     `json:"slug"`
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	PipelineCount int        `json:"pipeline_count"`
	LatestRunAt   *time.Time `json:"latest_run_at,omitempty"`
}

type PipelineSummary struct {
	ID                uuid.UUID `json:"id"`
	Name              string    `json:"name"`
	DefinitionVersion int       `json:"definition_version"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type RunSummary struct {
	ID           uuid.UUID  `json:"id"`
	PipelineID   uuid.UUID  `json:"pipeline_id"`
	PipelineName string     `json:"pipeline_name"`
	Counter      int64      `json:"counter"`
	Cause        string     `json:"cause"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	TriggeredBy  string     `json:"triggered_by,omitempty"`
}

type ProjectDetail struct {
	Project   ProjectSummary    `json:"project"`
	Pipelines []PipelineSummary `json:"pipelines"`
	Runs      []RunSummary      `json:"runs"`
}

type JobDetail struct {
	ID         uuid.UUID        `json:"id"`
	StageRunID uuid.UUID        `json:"stage_run_id"`
	Name       string           `json:"name"`
	MatrixKey  string           `json:"matrix_key,omitempty"`
	Image      string           `json:"image,omitempty"`
	Status     string           `json:"status"`
	ExitCode   *int32           `json:"exit_code,omitempty"`
	Error      string           `json:"error,omitempty"`
	StartedAt  *time.Time       `json:"started_at,omitempty"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
	AgentID    *uuid.UUID       `json:"agent_id,omitempty"`
	Logs       []LogLineSummary `json:"logs,omitempty"`
}

type StageDetail struct {
	ID         uuid.UUID   `json:"id"`
	Name       string      `json:"name"`
	Ordinal    int         `json:"ordinal"`
	Status     string      `json:"status"`
	StartedAt  *time.Time  `json:"started_at,omitempty"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
	Jobs       []JobDetail `json:"jobs"`
}

type RunDetail struct {
	RunSummary
	ProjectSlug string          `json:"project_slug"`
	CauseDetail json.RawMessage `json:"cause_detail,omitempty"`
	Revisions   json.RawMessage `json:"revisions,omitempty"`
	Stages      []StageDetail   `json:"stages"`
}

type LogLineSummary struct {
	Seq    int64     `json:"seq"`
	Stream string    `json:"stream"`
	At     time.Time `json:"at"`
	Text   string    `json:"text"`
}

// ListProjects returns every project with its pipeline count and the most
// recent run's created_at (nil if the project has never been triggered).
func (s *Store) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	rows, err := s.q.ListProjectsWithCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	out := make([]ProjectSummary, 0, len(rows))
	for _, r := range rows {
		p := ProjectSummary{
			ID:            fromPgUUID(r.ID),
			Slug:          r.Slug,
			Name:          r.Name,
			Description:   stringValue(r.Description),
			CreatedAt:     r.CreatedAt.Time,
			UpdatedAt:     r.UpdatedAt.Time,
			PipelineCount: int(r.PipelineCount),
		}
		if r.LatestRunAt.Valid {
			t := r.LatestRunAt.Time
			p.LatestRunAt = &t
		}
		out = append(out, p)
	}
	return out, nil
}

// GetProjectDetail bundles the project, its pipelines, and the most recent
// `runLimit` runs across those pipelines — the shape the project page needs
// in one call.
func (s *Store) GetProjectDetail(ctx context.Context, slug string, runLimit int32) (ProjectDetail, error) {
	proj, err := s.q.GetProjectBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProjectDetail{}, ErrProjectNotFound
		}
		return ProjectDetail{}, fmt.Errorf("store: get project: %w", err)
	}

	pipes, err := s.q.ListPipelinesByProjectSlug(ctx, slug)
	if err != nil {
		return ProjectDetail{}, fmt.Errorf("store: list pipelines: %w", err)
	}
	pipelineCount := len(pipes)

	if runLimit <= 0 {
		runLimit = 20
	}
	runs, err := s.q.ListRunsByProjectSlug(ctx, db.ListRunsByProjectSlugParams{
		Slug: slug, Limit: runLimit,
	})
	if err != nil {
		return ProjectDetail{}, fmt.Errorf("store: list runs: %w", err)
	}

	detail := ProjectDetail{
		Project: ProjectSummary{
			ID:            fromPgUUID(proj.ID),
			Slug:          proj.Slug,
			Name:          proj.Name,
			Description:   stringValue(proj.Description),
			CreatedAt:     proj.CreatedAt.Time,
			UpdatedAt:     proj.UpdatedAt.Time,
			PipelineCount: pipelineCount,
		},
	}
	for _, pl := range pipes {
		detail.Pipelines = append(detail.Pipelines, PipelineSummary{
			ID:                fromPgUUID(pl.ID),
			Name:              pl.Name,
			DefinitionVersion: int(pl.DefinitionVersion),
			UpdatedAt:         pl.UpdatedAt.Time,
		})
	}
	for _, r := range runs {
		detail.Runs = append(detail.Runs, RunSummary{
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
		})
		if len(detail.Runs) == 1 && detail.Project.LatestRunAt == nil {
			t := r.CreatedAt.Time
			detail.Project.LatestRunAt = &t
		}
	}
	return detail, nil
}

// GetRunDetail returns the run + all stages + all jobs + tail logs per job.
// logsPerJob caps lines per job; 0 disables log fetching (UI falls back to
// the run page's "load logs" action, not yet built).
func (s *Store) GetRunDetail(ctx context.Context, runID uuid.UUID, logsPerJob int32) (RunDetail, error) {
	run, err := s.q.GetRunWithPipeline(ctx, pgUUID(runID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RunDetail{}, ErrRunNotFound
		}
		return RunDetail{}, fmt.Errorf("store: get run: %w", err)
	}

	stages, err := s.q.ListStageRunsByRunOrdered(ctx, pgUUID(runID))
	if err != nil {
		return RunDetail{}, fmt.Errorf("store: list stages: %w", err)
	}
	jobs, err := s.q.ListJobRunsByRunFull(ctx, pgUUID(runID))
	if err != nil {
		return RunDetail{}, fmt.Errorf("store: list jobs: %w", err)
	}

	detail := RunDetail{
		RunSummary: RunSummary{
			ID:           fromPgUUID(run.ID),
			PipelineID:   fromPgUUID(run.PipelineID),
			PipelineName: run.PipelineName,
			Counter:      run.Counter,
			Cause:        run.Cause,
			Status:       run.Status,
			CreatedAt:    run.CreatedAt.Time,
			StartedAt:    pgTimePtr(run.StartedAt),
			FinishedAt:   pgTimePtr(run.FinishedAt),
			TriggeredBy:  stringValue(run.TriggeredBy),
		},
		ProjectSlug: run.ProjectSlug,
		CauseDetail: run.CauseDetail,
		Revisions:   run.Revisions,
	}

	jobsByStage := map[uuid.UUID][]JobDetail{}
	for _, j := range jobs {
		jd := JobDetail{
			ID:         fromPgUUID(j.ID),
			StageRunID: fromPgUUID(j.StageRunID),
			Name:       j.Name,
			MatrixKey:  stringValue(j.MatrixKey),
			Image:      stringValue(j.Image),
			Status:     j.Status,
			ExitCode:   j.ExitCode,
			Error:      stringValue(j.Error),
			StartedAt:  pgTimePtr(j.StartedAt),
			FinishedAt: pgTimePtr(j.FinishedAt),
		}
		if j.AgentID.Valid {
			aid := fromPgUUID(j.AgentID)
			jd.AgentID = &aid
		}
		if logsPerJob > 0 {
			logs, err := s.q.TailLogLinesByJob(ctx, db.TailLogLinesByJobParams{
				JobRunID: j.ID, Limit: logsPerJob,
			})
			if err != nil {
				return RunDetail{}, fmt.Errorf("store: tail logs: %w", err)
			}
			jd.Logs = make([]LogLineSummary, 0, len(logs))
			for _, l := range logs {
				jd.Logs = append(jd.Logs, LogLineSummary{
					Seq: l.Seq, Stream: l.Stream, At: l.At.Time, Text: l.Text,
				})
			}
		}
		jobsByStage[jd.StageRunID] = append(jobsByStage[jd.StageRunID], jd)
	}

	for _, st := range stages {
		sd := StageDetail{
			ID:         fromPgUUID(st.ID),
			Name:       st.Name,
			Ordinal:    int(st.Ordinal),
			Status:     st.Status,
			StartedAt:  pgTimePtr(st.StartedAt),
			FinishedAt: pgTimePtr(st.FinishedAt),
			Jobs:       jobsByStage[fromPgUUID(st.ID)],
		}
		detail.Stages = append(detail.Stages, sd)
	}
	return detail, nil
}
