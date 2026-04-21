package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
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
	// ConfigPath surfaces the repo-relative folder that holds
	// pipeline YAMLs — the edit dialog prefills its "Config
	// folder" input with this so round-tripping through Apply
	// preserves non-default values (".woodpecker", nested apps).
	ConfigPath    string     `json:"config_path,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	PipelineCount int        `json:"pipeline_count"`
	RunCount      int64      `json:"run_count"`
	LatestRunAt   *time.Time `json:"latest_run_at,omitempty"`
	// Provider: "github" | "gitlab" | "bitbucket" | "manual" | ""
	// (empty → no scm_source bound). The card pulls the icon from
	// this, and the filter pill groups rows by it.
	Provider string `json:"provider,omitempty"`
	// Status is a coarse health bucket derived from the latest run
	// per pipeline. Values: no_pipelines | never_run | running |
	// failing | success. The UI surfaces the same set as filter
	// chips at the top of the projects page.
	Status string `json:"status"`
	// TopPipelines is a preview-size slice of the project's pipelines
	// (at most 3, ordered by latest_run_at desc) for the card body.
	// Populated by ListProjects via ListTopPipelinesPerProject; empty
	// when the project has no pipelines yet.
	TopPipelines []PipelinePreview `json:"top_pipelines,omitempty"`
}

// PipelinePreview is the shape shown inside a project card: name,
// latest-run status, plus enough stage data for the horizontal
// pills the card renders. Empty LatestRunStatus means "never run"
// — in that case DefinitionStages provides the names the UI shows
// as grey pending pills.
type PipelinePreview struct {
	ID              uuid.UUID  `json:"id"`
	Name            string     `json:"name"`
	LatestRunStatus string     `json:"latest_run_status,omitempty"`
	LatestRunAt     *time.Time `json:"latest_run_at,omitempty"`
	// DefinitionStages are the ordered stage names pulled from
	// the YAML snapshot — used as the canonical list the card
	// iterates over. Empty when the pipeline's definition is
	// malformed (fallback: LatestRunStages only).
	DefinitionStages []string `json:"definition_stages,omitempty"`
	// LatestRunStages mirror stage_runs for the pipeline's latest
	// run in ordinal order. Keyed on stage name so the card can
	// overlay stage-run status onto the DefinitionStages pill list.
	LatestRunStages []StageRunSummary `json:"latest_run_stages,omitempty"`
}

// ProjectSCMInfo is the non-secret shape of an scm_source binding
// as seen by read paths: enough for the edit dialog to prefill
// provider/url/default_branch/auth_ref. Webhook ciphertext never
// rides along — rotation is the only way to produce a plaintext.
type ProjectSCMInfo struct {
	ID            uuid.UUID `json:"id"`
	Provider      string    `json:"provider"`
	URL           string    `json:"url"`
	DefaultBranch string    `json:"default_branch"`
	AuthRef       string    `json:"auth_ref,omitempty"`
}

type PipelineSummary struct {
	ID                uuid.UUID `json:"id"`
	Name              string    `json:"name"`
	DefinitionVersion int       `json:"definition_version"`
	UpdatedAt         time.Time `json:"updated_at"`
	// DefinitionStages are the ordered stage names from the parsed
	// pipeline YAML. The card uses them as grey pills when the
	// pipeline has never run, and to show "not executed yet"
	// stages after a partial run.
	DefinitionStages []string `json:"definition_stages,omitempty"`
	// DefinitionJobs lists every job in the YAML with its owning
	// stage — drives the greyed-out job rows inside stage boxes
	// for pipelines without runtime data yet.
	DefinitionJobs []DefinitionJob `json:"definition_jobs,omitempty"`
	// LatestRun is the most recent run for this pipeline across
	// all causes — nil when the pipeline has never been triggered.
	LatestRun *RunSummary `json:"latest_run,omitempty"`
	// LatestRunStages mirrors stage_runs rows for LatestRun.ID in
	// ordinal order, giving the UI per-stage status without a
	// second round-trip. Each stage carries its job_runs so the
	// GitLab-style pipeline flow can render a row per job.
	LatestRunStages []StageRunSummary `json:"latest_run_stages,omitempty"`
}

// StageRunSummary is the thin shape the pipeline card needs to
// render stage boxes — one box per entry, coloured by Status,
// with Jobs rendered inline as a vertical list à la GitLab CI.
type StageRunSummary struct {
	ID         uuid.UUID          `json:"id"`
	Name       string             `json:"name"`
	Ordinal    int                `json:"ordinal"`
	Status     string             `json:"status"`
	StartedAt  *time.Time         `json:"started_at,omitempty"`
	FinishedAt *time.Time         `json:"finished_at,omitempty"`
	Jobs       []JobRunSummaryLite `json:"jobs,omitempty"`
}

// JobRunSummaryLite is the render-side minimum: enough for the
// pipeline card to show a status-coloured row per job with the
// job name. Full JobDetail (logs, exit code) lives on the run
// detail page — this shape is intentionally thin so the card
// render isn't bloated with fields it doesn't use.
type JobRunSummaryLite struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// DefinitionJob pairs a job name with its stage — extracted from
// the pipeline YAML snapshot. Used to draw greyed-out job rows
// inside each stage box when the pipeline has never run (so the
// card still shows the shape operators can expect on first run).
type DefinitionJob struct {
	Name  string `json:"name"`
	Stage string `json:"stage"`
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
	// SCMSource is nil when the project has no repo bound yet —
	// the UI uses that to show the "Connect repo" affordance
	// instead of the edit-binding controls.
	SCMSource *ProjectSCMInfo   `json:"scm_source,omitempty"`
	Pipelines []PipelineSummary `json:"pipelines"`
	// Edges are the upstream-material relationships between
	// pipelines, mirrored from the VSM endpoint's edges. The
	// project page uses them to lay pipelines out as a DAG so
	// dependencies are visible at a glance instead of being
	// hidden under "upstream" in the YAML.
	Edges []PipelineEdge    `json:"edges,omitempty"`
	Runs  []RunSummary      `json:"runs"`
}

// PipelineEdge is a directed dependency: FromPipeline's Stage
// completing (optionally at Status) triggers ToPipeline. Names,
// not ids, because the upstream-material YAML references by name.
type PipelineEdge struct {
	FromPipeline string `json:"from_pipeline"`
	ToPipeline   string `json:"to_pipeline"`
	Stage        string `json:"stage,omitempty"`
	Status       string `json:"status,omitempty"`
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
	// Track an index so the top-pipelines merge is an O(1) lookup
	// instead of a scan per row.
	byID := make(map[uuid.UUID]int, len(rows))
	for _, r := range rows {
		id := fromPgUUID(r.ID)
		p := ProjectSummary{
			ID:            id,
			Slug:          r.Slug,
			Name:          r.Name,
			Description:   stringValue(r.Description),
			ConfigPath:    r.ConfigPath,
			CreatedAt:     r.CreatedAt.Time,
			UpdatedAt:     r.UpdatedAt.Time,
			PipelineCount: int(r.PipelineCount),
			RunCount:      r.RunCount,
			Provider:      r.Provider,
			Status:        r.StatusAgg,
		}
		if r.LatestRunAt.Valid {
			t := r.LatestRunAt.Time
			p.LatestRunAt = &t
		}
		byID[id] = len(out)
		out = append(out, p)
	}

	// Second query: the preview stack per project. One round-trip
	// regardless of project count — ROW_NUMBER caps to 3 per partition.
	// Failures here are non-fatal: the card still renders without the
	// preview, just shows pipeline_count alone.
	tops, err := s.q.ListTopPipelinesPerProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list top pipelines: %w", err)
	}
	// Indexing for the stage-runs overlay: map from run_id back to
	// the (projectIdx, previewIdx) slot we need to attach the
	// stage_runs onto. Zero-UUID sentinels (never-run) are skipped
	// so the batch query only asks about real runs.
	type slot struct{ proj, prev int }
	runToSlot := make(map[uuid.UUID]slot)
	var runIDs []pgtype.UUID
	for _, t := range tops {
		projectID := fromPgUUID(t.ProjectID)
		idx, ok := byID[projectID]
		if !ok {
			continue
		}
		preview := PipelinePreview{
			ID:   fromPgUUID(t.PipelineID),
			Name: t.Name,
			// LatestRunStatus comes back as "" when the pipeline has
			// never run — sqlc types the LEFT JOIN LATERAL column as
			// non-nullable because runs.status itself is NOT NULL, so
			// "never run" surfaces here as empty rather than *nil.
			LatestRunStatus: t.LatestRunStatus,
		}
		if t.LatestRunAt.Valid {
			ts := t.LatestRunAt.Time
			preview.LatestRunAt = &ts
		}
		// Stage names from the parsed YAML. The card uses these
		// even for never-run pipelines so the pill strip isn't
		// empty. Parse errors leave the slice nil — UI falls back
		// to rendering only whatever came back in stage_runs.
		var def struct {
			Stages []string `json:"Stages"`
		}
		if err := json.Unmarshal(t.Definition, &def); err == nil {
			preview.DefinitionStages = def.Stages
		}
		prevIdx := len(out[idx].TopPipelines)
		out[idx].TopPipelines = append(out[idx].TopPipelines, preview)

		runID := fromPgUUID(t.LatestRunID)
		if runID != uuid.Nil {
			runToSlot[runID] = slot{proj: idx, prev: prevIdx}
			runIDs = append(runIDs, t.LatestRunID)
		}
	}

	// Third query: stage_runs for every latest run in the preview
	// stack. Single batch regardless of project/pipeline count,
	// matching the pattern GetProjectDetail already uses for the
	// pipeline cards on the detail page.
	if len(runIDs) > 0 {
		stageRows, err := s.q.ListStageRunsForRuns(ctx, runIDs)
		if err != nil {
			return nil, fmt.Errorf("store: list project-card stage runs: %w", err)
		}
		for _, sr := range stageRows {
			slot, ok := runToSlot[fromPgUUID(sr.RunID)]
			if !ok {
				continue
			}
			out[slot.proj].TopPipelines[slot.prev].LatestRunStages = append(
				out[slot.proj].TopPipelines[slot.prev].LatestRunStages,
				StageRunSummary{
					ID:         fromPgUUID(sr.ID),
					Name:       sr.Name,
					Ordinal:    int(sr.Ordinal),
					Status:     sr.Status,
					StartedAt:  pgTimePtr(sr.StartedAt),
					FinishedAt: pgTimePtr(sr.FinishedAt),
				},
			)
		}
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
			ConfigPath:    proj.ConfigPath,
			CreatedAt:     proj.CreatedAt.Time,
			UpdatedAt:     proj.UpdatedAt.Time,
			PipelineCount: pipelineCount,
		},
		// Pre-allocate to empty slices so the JSON stays `[]` instead of
		// `null` when there are no rows. Clients iterate without nil-guards.
		Pipelines: []PipelineSummary{},
		Runs:      []RunSummary{},
	}
	// Index base rows by pipeline id — the enrichment steps below
	// need fast lookup to attach latest-run + stage-run info to
	// the right card in a single pass over the pipes slice.
	pipelineIdx := make(map[uuid.UUID]int, len(pipes))
	for _, pl := range pipes {
		id := fromPgUUID(pl.ID)
		pipelineIdx[id] = len(detail.Pipelines)
		card := PipelineSummary{
			ID:                id,
			Name:              pl.Name,
			DefinitionVersion: int(pl.DefinitionVersion),
			UpdatedAt:         pl.UpdatedAt.Time,
		}
		// The stored JSONB is a marshalled domain.Pipeline. We
		// read Stages for ordering and Jobs to populate the
		// per-stage job lists — parse errors leave both nil so
		// the UI falls back to an empty-state card rather than
		// 500-ing.
		var def struct {
			Stages []string `json:"Stages"`
			Jobs   []struct {
				Name  string `json:"Name"`
				Stage string `json:"Stage"`
			} `json:"Jobs"`
		}
		if err := json.Unmarshal(pl.Definition, &def); err == nil {
			card.DefinitionStages = def.Stages
			for _, j := range def.Jobs {
				card.DefinitionJobs = append(card.DefinitionJobs, DefinitionJob{
					Name:  j.Name,
					Stage: j.Stage,
				})
			}
		}
		detail.Pipelines = append(detail.Pipelines, card)
	}

	// Latest run per pipeline — the card shows status badge +
	// duration + counter pulled from here. Pipelines without a
	// run are skipped entirely so the field stays nil on the card.
	latestRuns, err := s.q.LatestRunPerPipelineByProjectSlug(ctx, slug)
	if err != nil {
		return ProjectDetail{}, fmt.Errorf("store: latest runs per pipeline: %w", err)
	}
	latestRunIDs := make([]pgtype.UUID, 0, len(latestRuns))
	latestRunByID := make(map[uuid.UUID]int, len(latestRuns))
	for _, lr := range latestRuns {
		pID := fromPgUUID(lr.PipelineID)
		idx, ok := pipelineIdx[pID]
		if !ok {
			continue
		}
		run := RunSummary{
			ID:           fromPgUUID(lr.ID),
			PipelineID:   pID,
			PipelineName: detail.Pipelines[idx].Name,
			Counter:      lr.Counter,
			Cause:        lr.Cause,
			Status:       lr.Status,
			CreatedAt:    lr.CreatedAt.Time,
			StartedAt:    pgTimePtr(lr.StartedAt),
			FinishedAt:   pgTimePtr(lr.FinishedAt),
			TriggeredBy:  stringValue(lr.TriggeredBy),
		}
		detail.Pipelines[idx].LatestRun = &run
		latestRunIDs = append(latestRunIDs, lr.ID)
		latestRunByID[run.ID] = idx
	}

	// Batch-load stage_runs for every latest run — one query
	// regardless of pipeline count, so adding cards doesn't scale
	// DB round-trips linearly.
	//
	// stagePos indexes (pipelineIdx, stageRunID) -> position in
	// LatestRunStages so the job_runs loop below can attach jobs
	// onto the right stage without rescanning.
	type stagePos struct{ pipeline, stage int }
	stageByID := make(map[uuid.UUID]stagePos)
	if len(latestRunIDs) > 0 {
		stageRows, err := s.q.ListStageRunsForRuns(ctx, latestRunIDs)
		if err != nil {
			return ProjectDetail{}, fmt.Errorf("store: list stage runs: %w", err)
		}
		for _, sr := range stageRows {
			rID := fromPgUUID(sr.RunID)
			idx, ok := latestRunByID[rID]
			if !ok {
				continue
			}
			stageID := fromPgUUID(sr.ID)
			stageByID[stageID] = stagePos{
				pipeline: idx,
				stage:    len(detail.Pipelines[idx].LatestRunStages),
			}
			detail.Pipelines[idx].LatestRunStages = append(
				detail.Pipelines[idx].LatestRunStages,
				StageRunSummary{
					ID:         stageID,
					Name:       sr.Name,
					Ordinal:    int(sr.Ordinal),
					Status:     sr.Status,
					StartedAt:  pgTimePtr(sr.StartedAt),
					FinishedAt: pgTimePtr(sr.FinishedAt),
				},
			)
		}

		// Same batch pattern for job_runs. Everything groups by
		// stage_run_id — the card renders a row per job inside
		// each stage box.
		jobRows, err := s.q.ListJobRunsForRuns(ctx, latestRunIDs)
		if err != nil {
			return ProjectDetail{}, fmt.Errorf("store: list job runs: %w", err)
		}
		for _, jr := range jobRows {
			pos, ok := stageByID[fromPgUUID(jr.StageRunID)]
			if !ok {
				continue
			}
			detail.Pipelines[pos.pipeline].LatestRunStages[pos.stage].Jobs = append(
				detail.Pipelines[pos.pipeline].LatestRunStages[pos.stage].Jobs,
				JobRunSummaryLite{
					ID:         fromPgUUID(jr.ID),
					Name:       jr.Name,
					Status:     jr.Status,
					StartedAt:  pgTimePtr(jr.StartedAt),
					FinishedAt: pgTimePtr(jr.FinishedAt),
				},
			)
		}
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

	// Edges: materials with type='upstream' encode cross-pipeline
	// triggers, which the project page lays out as a DAG. Git
	// materials aren't relevant here — those are entry points the
	// pipeline pulls from, not dependencies to other pipelines.
	mats, err := s.q.ListMaterialsByProjectSlug(ctx, slug)
	if err != nil {
		return ProjectDetail{}, fmt.Errorf("store: list materials for detail: %w", err)
	}
	// Build id→name once so we can label edges by pipeline name
	// (the YAML upstream reference already uses names, keep the
	// wire shape consistent).
	pipelineNameByID := make(map[uuid.UUID]string, len(pipes))
	for _, pl := range pipes {
		pipelineNameByID[fromPgUUID(pl.ID)] = pl.Name
	}
	for _, m := range mats {
		if m.Type != string(domain.MaterialUpstream) {
			continue
		}
		var u struct {
			Pipeline string `json:"pipeline"`
			Stage    string `json:"stage,omitempty"`
			Status   string `json:"status,omitempty"`
		}
		if err := json.Unmarshal(m.Config, &u); err != nil {
			continue
		}
		toName, ok := pipelineNameByID[fromPgUUID(m.PipelineID)]
		if !ok {
			continue
		}
		detail.Edges = append(detail.Edges, PipelineEdge{
			FromPipeline: u.Pipeline,
			ToPipeline:   toName,
			Stage:        u.Stage,
			Status:       u.Status,
		})
	}

	// Bind info is optional — projects scaffolded via the Empty
	// or Template tab never get an scm_source. Swallow the
	// not-found sentinel silently; anything else logs + nils out
	// the field so detail still renders without the binding
	// (the UI's edit dialog shows the connect-repo tab).
	scm, err := s.FindSCMSourceByProjectSlug(ctx, slug)
	if err == nil {
		detail.SCMSource = &ProjectSCMInfo{
			ID:            scm.ID,
			Provider:      scm.Provider,
			URL:           scm.URL,
			DefaultBranch: scm.DefaultBranch,
			AuthRef:       scm.AuthRef,
		}
	} else if !errors.Is(err, ErrSCMSourceNotFound) {
		return ProjectDetail{}, fmt.Errorf("store: detail scm_source: %w", err)
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
		Stages:      []StageDetail{},
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
		jobs := jobsByStage[fromPgUUID(st.ID)]
		if jobs == nil {
			jobs = []JobDetail{}
		}
		sd := StageDetail{
			ID:         fromPgUUID(st.ID),
			Name:       st.Name,
			Ordinal:    int(st.Ordinal),
			Status:     st.Status,
			StartedAt:  pgTimePtr(st.StartedAt),
			FinishedAt: pgTimePtr(st.FinishedAt),
			Jobs:       jobs,
		}
		detail.Stages = append(detail.Stages, sd)
	}
	return detail, nil
}
