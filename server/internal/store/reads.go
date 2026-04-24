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
	// Metrics aggregate the per-pipeline metrics across the project
	// with runs-weighted averages — the projects list card shows
	// success/p50/runs at the project scope without the UI having
	// to compute them. Nil when the project has no terminal runs.
	Metrics *PipelineMetrics `json:"metrics,omitempty"`
	// LatestRunMeta is the commit info of the most recently kicked
	// run across the project's pipelines — branch/sha/message/author
	// — so the card can show the commit line without a second
	// roundtrip to project detail.
	LatestRunMeta *RunMeta `json:"latest_run_meta,omitempty"`
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
	// PollIntervalNs is the project-level poll fallback in
	// nanoseconds. Zero disables; wire shape stays machine-
	// friendly so the UI formats via Go-style duration without
	// re-encoding.
	PollIntervalNs int64 `json:"poll_interval_ns,omitempty"`
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
	// Metrics are aggregate stats over a rolling window (7 days
	// by default). Nil when no terminal run exists in the window
	// — the UI degrades gracefully to "not enough data" instead
	// of rendering zeroed-out medians as legitimate values.
	Metrics *PipelineMetrics `json:"metrics,omitempty"`
	// LatestRunMeta carries the commit metadata (branch, message,
	// author, revision) that triggered the latest run — lifted
	// out of runs.revisions JSONB + modifications join so the
	// card header can show "feat/idempotency-v2 · fix: idempotency
	// key leak" without a second round-trip.
	LatestRunMeta *RunMeta `json:"latest_run_meta,omitempty"`
}

type RunMeta struct {
	Revision string `json:"revision,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Message  string `json:"message,omitempty"`
	Author   string `json:"author,omitempty"`
	// TriggeredBy is the user or system that kicked the run off.
	// For webhook-driven runs Author usually carries the real
	// identity (git commit author), but manual triggers have no
	// modification row → Author is empty and the UI falls back to
	// TriggeredBy so the card still shows "who did this".
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// PipelineMetrics feeds the card's footer strip: lead time (wall
// clock), process time (summed busy stages), success rate across
// recent terminal runs. StageStats drives the per-stage duration
// badge + bottleneck call-out.
type PipelineMetrics struct {
	WindowDays        int         `json:"window_days"`
	RunsConsidered    int         `json:"runs_considered"`
	SuccessRate       float64     `json:"success_rate"`
	LeadTimeP50Sec    float64     `json:"lead_time_p50_seconds"`
	ProcessTimeP50Sec float64     `json:"process_time_p50_seconds"`
	StageStats        []StageStat `json:"stage_stats,omitempty"`
}

type StageStat struct {
	Name           string  `json:"name"`
	RunsConsidered int     `json:"runs_considered"`
	SuccessRate    float64 `json:"success_rate"`
	DurationP50Sec float64 `json:"duration_p50_seconds"`
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

	// Approval gate fields. Populated only when approval_gate is
	// true so regular jobs don't carry dead JSON on every response.
	ApprovalGate        bool       `json:"approval_gate,omitempty"`
	Approvers           []string   `json:"approvers,omitempty"`
	ApprovalDescription string     `json:"approval_description,omitempty"`
	AwaitingSince       *time.Time `json:"awaiting_since,omitempty"`
	DecidedBy           string     `json:"decided_by,omitempty"`
	DecidedAt           *time.Time `json:"decided_at,omitempty"`
	Decision            string     `json:"decision,omitempty"`

	// Notification metadata — populated only for synthetic jobs
	// in the `_notifications` stage. The UI keys off NotifyOn
	// to render the trigger pill and NotifyUses for the friendly
	// job label (the raw `_notify_<idx>` name isn't meant to
	// leak into the timeline).
	NotifyOn   string `json:"notify_on,omitempty"`
	NotifyUses string `json:"notify_uses,omitempty"`
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
	// pipeline cards on the detail page. Also indexes (stage_run_id
	// → preview slot) so the job_runs pass below can attach jobs on
	// top of the stage_runs without rescanning.
	type stagePos struct{ proj, prev, stage int }
	stageByID := make(map[uuid.UUID]stagePos)
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
			stageID := fromPgUUID(sr.ID)
			stageIdx := len(out[slot.proj].TopPipelines[slot.prev].LatestRunStages)
			stageByID[stageID] = stagePos{
				proj: slot.proj, prev: slot.prev, stage: stageIdx,
			}
			out[slot.proj].TopPipelines[slot.prev].LatestRunStages = append(
				out[slot.proj].TopPipelines[slot.prev].LatestRunStages,
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

		// Batch job_runs across the same set of latest runs. Without
		// this the projects list renders empty job-dot rows — the
		// stages strip design (one circle per job) depends on the
		// job_runs being attached to their stage_runs here.
		jobRows, err := s.q.ListJobRunsForRuns(ctx, runIDs)
		if err != nil {
			return nil, fmt.Errorf("store: list project-card job runs: %w", err)
		}
		for _, jr := range jobRows {
			pos, ok := stageByID[fromPgUUID(jr.StageRunID)]
			if !ok {
				continue
			}
			stage := &out[pos.proj].TopPipelines[pos.prev].LatestRunStages[pos.stage]
			stage.Jobs = append(stage.Jobs, JobRunSummaryLite{
				ID:         fromPgUUID(jr.ID),
				Name:       jr.Name,
				Status:     jr.Status,
				StartedAt:  pgTimePtr(jr.StartedAt),
				FinishedAt: pgTimePtr(jr.FinishedAt),
			})
		}
	}

	// Fourth query: per-project aggregate metrics. One scan for the
	// full list — failures here are non-fatal, projects without a
	// terminal run simply lack Metrics (the card falls back to "no
	// data yet" text). Same window as the per-pipeline metrics.
	metricRows, err := s.q.ProjectMetricsAggregated(ctx, intervalDays(MetricsWindowDays))
	if err != nil {
		return nil, fmt.Errorf("store: project metrics: %w", err)
	}
	for _, r := range metricRows {
		idx, ok := byID[fromPgUUID(r.ProjectID)]
		if !ok || r.RunsConsidered == 0 {
			continue
		}
		out[idx].Metrics = &PipelineMetrics{
			WindowDays:        MetricsWindowDays,
			RunsConsidered:    int(r.RunsConsidered),
			SuccessRate:       float64(r.Passed) / float64(r.RunsConsidered),
			LeadTimeP50Sec:    r.LeadTimeP50S,
			ProcessTimeP50Sec: r.ProcessTimeP50S,
		}
	}

	// Fifth query: per-project latest run commit metadata (branch,
	// message, author, revision). Same JSONB-expand pattern as the
	// per-slug variant, fans out once for the whole projects list.
	metaRows, err := s.q.LatestRunMetaPerProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: latest run meta per project: %w", err)
	}
	for _, m := range metaRows {
		idx, ok := byID[fromPgUUID(m.ProjectID)]
		if !ok {
			continue
		}
		out[idx].LatestRunMeta = &RunMeta{
			Revision:    m.Revision,
			Branch:      m.Branch,
			Message:     stringValue(m.Message),
			Author:      stringValue(m.Author),
			TriggeredBy: stringValue(m.TriggeredBy),
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

	// Pipeline metrics — aggregate stats over the 7-day window fuel
	// the card footer (lead/process p50, success rate) and the per
	// stage call-outs. Failures here are non-fatal: the card still
	// renders without the strip, just without the metrics.
	if err := s.attachPipelineMetrics(ctx, slug, detail.Pipelines, pipelineIdx); err != nil {
		return ProjectDetail{}, fmt.Errorf("store: pipeline metrics: %w", err)
	}

	// Commit metadata for the latest run — one extra query, same
	// pattern as metrics: failures bubble up so the endpoint is
	// transparent about partial data instead of silently omitting
	// the branch/commit pill.
	metaRows, err := s.q.LatestRunMetaByProjectSlug(ctx, slug)
	if err != nil {
		return ProjectDetail{}, fmt.Errorf("store: latest run meta: %w", err)
	}
	for _, m := range metaRows {
		idx, ok := pipelineIdx[fromPgUUID(m.PipelineID)]
		if !ok {
			continue
		}
		detail.Pipelines[idx].LatestRunMeta = &RunMeta{
			Revision:    m.Revision,
			Branch:      m.Branch,
			Message:     stringValue(m.Message),
			Author:      stringValue(m.Author),
			TriggeredBy: stringValue(m.TriggeredBy),
		}
	}

	// Bind info is optional — projects scaffolded via the Empty
	// or Template tab never get an scm_source. Swallow the
	// not-found sentinel silently; anything else logs + nils out
	// the field so detail still renders without the binding
	// (the UI's edit dialog shows the connect-repo tab).
	scm, err := s.FindSCMSourceByProjectSlug(ctx, slug)
	if err == nil {
		detail.SCMSource = &ProjectSCMInfo{
			ID:             scm.ID,
			Provider:       scm.Provider,
			URL:            scm.URL,
			DefaultBranch:  scm.DefaultBranch,
			AuthRef:        scm.AuthRef,
			PollIntervalNs: int64(scm.PollInterval),
		}
	} else if !errors.Is(err, ErrSCMSourceNotFound) {
		return ProjectDetail{}, fmt.Errorf("store: detail scm_source: %w", err)
	}

	return detail, nil
}

// GetRunDetail returns the run + all stages + all jobs + tail logs per job.
// logsPerJob caps lines per job; 0 disables log fetching (UI falls back to
// the run page's "load logs" action, not yet built).
//
// When `since` carries a cursor for a job_run_id, only log lines
// with `seq > cursor` are returned for that job (ordered oldest-
// first, capped at logsPerJob lines). Jobs missing from the map
// fall back to the tail behaviour. Lets the polling client pull
// pure deltas instead of re-fetching the last N lines every tick
// — survives bursty jobs that produce >N lines between polls,
// which the tail-only path silently drops.
func (s *Store) GetRunDetail(ctx context.Context, runID uuid.UUID, logsPerJob int32, since map[uuid.UUID]int64) (RunDetail, error) {
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

	// Decode the pipeline definition just once — only the
	// notifications array matters here. Failure is non-fatal:
	// worst case the UI sees raw `_notify_<idx>` names, which
	// is the same degradation as before this polish.
	var pipelineNotifications []domain.Notification
	if len(run.PipelineDefinition) > 0 {
		var def struct {
			Notifications []domain.Notification `json:"Notifications"`
		}
		if err := json.Unmarshal(run.PipelineDefinition, &def); err == nil {
			pipelineNotifications = def.Notifications
		}
	}

	jobsByStage := map[uuid.UUID][]JobDetail{}
	for _, j := range jobs {
		jd := JobDetail{
			ID:                  fromPgUUID(j.ID),
			StageRunID:          fromPgUUID(j.StageRunID),
			Name:                j.Name,
			MatrixKey:           stringValue(j.MatrixKey),
			Image:               stringValue(j.Image),
			Status:              j.Status,
			ExitCode:            j.ExitCode,
			Error:               stringValue(j.Error),
			StartedAt:           pgTimePtr(j.StartedAt),
			FinishedAt:          pgTimePtr(j.FinishedAt),
			ApprovalGate:        j.ApprovalGate,
			Approvers:           j.Approvers,
			ApprovalDescription: stringValue(j.ApprovalDescription),
			AwaitingSince:       pgTimePtr(j.AwaitingSince),
			DecidedBy:           stringValue(j.DecidedBy),
			DecidedAt:           pgTimePtr(j.DecidedAt),
			Decision:            stringValue(j.Decision),
		}
		if j.AgentID.Valid {
			aid := fromPgUUID(j.AgentID)
			jd.AgentID = &aid
		}
		if logsPerJob > 0 {
			var logs []LogLineSummary
			jobUUID := fromPgUUID(j.ID)
			if cursor, has := since[jobUUID]; has {
				// Delta fetch: lines strictly after the cursor,
				// oldest-first, capped at logsPerJob.
				logs, err = s.logLinesAfterSeq(ctx, jobUUID, cursor, int64(logsPerJob))
			} else {
				// Tail fetch (initial load / no cursor yet):
				// last N lines, oldest-first within the window.
				rows, tailErr := s.q.TailLogLinesByJob(ctx, db.TailLogLinesByJobParams{
					JobRunID: j.ID, Limit: logsPerJob,
				})
				if tailErr != nil {
					return RunDetail{}, fmt.Errorf("store: tail logs: %w", tailErr)
				}
				logs = make([]LogLineSummary, 0, len(rows))
				for _, l := range rows {
					logs = append(logs, LogLineSummary{
						Seq: l.Seq, Stream: l.Stream, At: l.At.Time, Text: l.Text,
					})
				}
			}
			if err != nil {
				return RunDetail{}, fmt.Errorf("store: logs: %w", err)
			}
			jd.Logs = logs
		}
		// Synthetic notification job: stamp the human-readable
		// trigger + plugin ref so the UI can render a real label
		// instead of the raw `_notify_<idx>` slug. Out-of-range
		// indices (definition drift after apply) leave the fields
		// empty and the UI falls back to the slug.
		if idx, ok := domain.NotificationIndexFromName(jd.Name); ok &&
			idx < len(pipelineNotifications) {
			n := pipelineNotifications[idx]
			jd.NotifyOn = string(n.On)
			jd.NotifyUses = n.Uses
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

// logLinesAfterSeq returns log lines strictly after `sinceSeq` for
// a given job_run, ordered oldest-first, capped at `limit`. Used
// by the polling client's delta-fetch path so a job that produces
// more lines than the tail window can keep stays streamable
// without losing the middle chunk. Raw SQL — a single query that
// doesn't warrant a sqlc entry alongside the existing
// TailLogLinesByJob.
func (s *Store) logLinesAfterSeq(ctx context.Context, jobRunID uuid.UUID, sinceSeq int64, limit int64) ([]LogLineSummary, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT seq, stream, at, text
		FROM log_lines
		WHERE job_run_id = $1 AND seq > $2
		ORDER BY seq ASC
		LIMIT $3
	`, jobRunID, sinceSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LogLineSummary, 0, 64)
	for rows.Next() {
		var l LogLineSummary
		var at time.Time
		if err := rows.Scan(&l.Seq, &l.Stream, &at, &l.Text); err != nil {
			return nil, err
		}
		l.At = at
		out = append(out, l)
	}
	return out, rows.Err()
}
