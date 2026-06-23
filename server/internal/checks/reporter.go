// Package checks reports run state transitions back to GitHub as
// Check Runs. Activated only when a GitHub App is configured AND the
// run was triggered by a webhook (push or pull_request) on a repo
// where the App is installed. Manual / upstream runs skip silently.
package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// Reporter is the glue between a run's lifecycle and the GitHub
// Checks API. Goroutine-spawning wrappers (ReportRunCreated /
// ReportRunCompleted) fire-and-forget so we never block the hot
// request path on a remote call. Any error is logged and swallowed.
//
// The App client is read through the vcs.Registry at call time,
// not captured at construction — that's what lets the admin UI
// rotate GitHub App credentials and have the next reported run
// pick them up without a server restart.
type Reporter struct {
	store      *store.Store
	vcs        *vcs.Registry
	publicBase string
	log        *slog.Logger
}

// NewReporter returns nil when store or publicBase is missing —
// callers treat a nil *Reporter as "feature disabled", so every
// call site is a simple `if r != nil { r.Report...() }`. Passing
// a registry with no github_app currently configured is fine:
// each call guards on appClient() and no-ops cleanly.
func NewReporter(s *store.Store, registry *vcs.Registry, publicBase string, log *slog.Logger) *Reporter {
	if s == nil || publicBase == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reporter{
		store:      s,
		vcs:        registry,
		publicBase: strings.TrimRight(publicBase, "/"),
		log:        log,
	}
}

// appClient returns the currently active GitHub App client, or
// nil when none is configured. Guarded by every public method
// that actually talks to GitHub.
func (r *Reporter) appClient() *ghscm.AppClient {
	if r == nil || r.vcs == nil {
		return nil
	}
	return r.vcs.GitHubApp()
}

// ReportRunCreated is called from the webhook path once a new run is
// queued. Fire-and-forget: spawns a goroutine so the caller's HTTP
// request returns immediately. The request's ctx is replaced by a
// 30s detached background so the work survives the response.
func (r *Reporter) ReportRunCreated(_ context.Context, runID uuid.UUID) {
	if r == nil {
		return
	}
	go func() {
		work, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.CreateCheck(work, runID); err != nil {
			r.log.Warn("checks: create failed", "run_id", runID, "err", err)
		}
	}()
}

// ReportRunCompleted is called from the JobResult handler when a
// run reaches terminal state. Same fire-and-forget pattern as
// ReportRunCreated; no-op when we never created a check for this
// run.
func (r *Reporter) ReportRunCompleted(_ context.Context, runID uuid.UUID, status string) {
	if r == nil {
		return
	}
	go func() {
		work, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.CompleteCheck(work, runID, status); err != nil {
			r.log.Warn("checks: update failed", "run_id", runID, "err", err)
		}
	}()
}

// ReportRunReopened is called from the rerun path (full run or single
// job). Fire-and-forget like the others. Re-opens the run's existing
// check to in_progress rather than creating a fresh one, so concurrent
// single-job reruns on the same run don't orphan check runs.
func (r *Reporter) ReportRunReopened(_ context.Context, runID uuid.UUID) {
	if r == nil {
		return
	}
	go func() {
		work, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.ReopenCheck(work, runID); err != nil {
			r.log.Warn("checks: reopen failed", "run_id", runID, "err", err)
		}
	}()
}

// CreateCheck is the synchronous version of ReportRunCreated —
// callable from tests and from any caller that wants to know whether
// the check was created. Returns nil when the run shouldn't produce
// a check (manual/upstream cause, non-GitHub repo, App not
// installed) so callers can't trivially tell "created" from
// "skipped"; check logs for that.
func (r *Reporter) CreateCheck(ctx context.Context, runID uuid.UUID) error {
	app := r.appClient()
	if app == nil {
		// Registry has no active github_app — admin deleted the
		// row or env+DB both empty. Treated like "feature
		// disabled": no work, no error.
		return nil
	}
	ctxInfo, err := r.resolveRunContext(ctx, runID)
	if err != nil {
		return err
	}
	if ctxInfo == nil {
		return nil // non-reportable cause, non-GitHub repo, etc.
	}
	installationID, err := app.InstallationID(ctx, ctxInfo.owner, ctxInfo.repo)
	if errors.Is(err, ghscm.ErrNoInstallation) {
		r.log.Info("checks: app not installed, skipping",
			"run_id", runID, "repo", ctxInfo.owner+"/"+ctxInfo.repo)
		return nil
	}
	if err != nil {
		return fmt.Errorf("installation lookup: %w", err)
	}

	created, err := app.CreateCheckRun(ctx, installationID, ghscm.CreateCheckRunInput{
		Owner:      ctxInfo.owner,
		Repo:       ctxInfo.repo,
		Name:       fmt.Sprintf("gocdnext / %s", ctxInfo.pipelineName),
		HeadSHA:    ctxInfo.headSHA,
		Status:     ghscm.CheckStatusInProgress,
		DetailsURL: r.detailsURL(runID),
		ExternalID: runID.String(),
		Output: &ghscm.CheckRunOutput{
			Title:   "Pipeline queued",
			Summary: fmt.Sprintf("Run #%d on %s — follow the run for details.", ctxInfo.counter, ctxInfo.branch),
		},
	})
	if err != nil {
		return fmt.Errorf("create check run: %w", err)
	}

	if err := r.store.UpsertGithubCheckRun(ctx, store.UpsertGithubCheckRunInput{
		RunID:          runID,
		InstallationID: installationID,
		CheckRunID:     created.ID,
		Owner:          ctxInfo.owner,
		Repo:           ctxInfo.repo,
		HeadSHA:        ctxInfo.headSHA,
	}); err != nil {
		return fmt.Errorf("persist check link: %w", err)
	}

	r.log.Info("checks: created",
		"run_id", runID, "check_run_id", created.ID,
		"repo", ctxInfo.owner+"/"+ctxInfo.repo, "head_sha", ctxInfo.headSHA)
	return nil
}

// CompleteCheck is the synchronous version of ReportRunCompleted.
// Returns nil when we have no check record for the run (feature
// disabled, or create-side skipped). Serialized per run (advisory lock)
// with ReopenCheck so a stale completion can't land between a concurrent
// reopen's status read and its PATCH.
func (r *Reporter) CompleteCheck(ctx context.Context, runID uuid.UUID, status string) error {
	if r.appClient() == nil {
		return nil
	}
	return r.store.WithRunCheckLock(ctx, runID, func() error {
		return r.completeCheckLocked(ctx, runID, status)
	})
}

// completeCheckLocked is CompleteCheck's body, run while holding the
// per-run check lock. reopenLocked's self-heal calls it directly (it
// already holds the lock) — re-acquiring would deadlock on a different
// pooled connection.
func (r *Reporter) completeCheckLocked(ctx context.Context, runID uuid.UUID, status string) error {
	app := r.appClient()
	if app == nil {
		return nil
	}
	link, err := r.store.GetGithubCheckRun(ctx, runID)
	if errors.Is(err, store.ErrCheckRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	// The run's CURRENT status is authoritative, not the status captured
	// when this completion was queued. A completion can arrive stale: the
	// original run's terminal fired async, but the user has since re-run a
	// job — re-opening THIS SAME check. Completing now with the stale
	// status would flip the PR back to red mid-rerun. So re-read: skip
	// while the run is non-terminal (a rerun is in flight), else use the
	// fresh status. Idempotent with ReopenCheck's self-heal — both route
	// through here, so whichever runs last writes the same state.
	current, terminal, err := r.runTerminalStatus(ctx, runID)
	if err != nil {
		return err
	}
	if !terminal {
		r.log.Info("checks: skipping stale completion — run re-opened",
			"run_id", runID, "queued_status", status, "current_status", current)
		return nil
	}
	status = current

	conclusion := conclusionFor(status)
	title := "Pipeline " + status
	summary := fmt.Sprintf("gocdnext run finished with status=%s.", status)
	// Coverage enrichment: when the run reported coverage, the
	// check summary carries the per-series percentages + delta vs
	// the mainline baseline — the number a PR reviewer wants
	// without leaving GitHub. Best-effort: a lookup failure
	// degrades to the plain summary, never blocks the check.
	if covLine := r.coverageSummaryLine(ctx, runID); covLine != "" {
		summary += "\n\n" + covLine
	}

	if err := app.UpdateCheckRun(ctx, link.InstallationID, ghscm.UpdateCheckRunInput{
		Owner:      link.Owner,
		Repo:       link.Repo,
		CheckRunID: link.CheckRunID,
		Status:     ghscm.CheckStatusCompleted,
		Conclusion: conclusion,
		Output: &ghscm.CheckRunOutput{
			Title:   title,
			Summary: summary,
		},
	}); err != nil {
		return fmt.Errorf("patch check run: %w", err)
	}
	r.log.Info("checks: updated",
		"run_id", runID, "check_run_id", link.CheckRunID,
		"status", status, "conclusion", conclusion)
	// Record that this check run is now terminal so a later rerun recreates
	// it instead of reusing it — GitHub won't cleanly reopen a completed
	// check (completed_at is set-once). Best-effort: on failure the next
	// rerun reuses (the old behaviour), which is degraded, not broken.
	if err := r.store.MarkGithubCheckRunCompleted(ctx, runID); err != nil {
		r.log.Warn("checks: mark completed failed",
			"run_id", runID, "check_run_id", link.CheckRunID, "err", err)
	}
	return nil
}

// ReopenCheck re-opens a run's check on a rerun. It REUSES the existing
// check run (one per run) when there is one — so a single-job rerun, or
// two concurrent job reruns on the same run, never orphan a check run or
// churn the run→check link (each just re-PATCHes the same check run to
// in_progress). When there's no prior check — a fresh run from a full
// rerun, or a non-reportable cause — it falls back to CreateCheck.
//
// It also self-heals the fire-and-forget race against ReportRunCompleted:
// a very fast rerun can reach terminal before (or while) we re-open,
// which would otherwise leave GitHub stuck at in_progress. After
// re-opening we re-read the run status and, if it's already terminal,
// complete the check immediately. Idempotent with the connect.go
// completion path — whichever closes it last writes the same conclusion.
func (r *Reporter) ReopenCheck(ctx context.Context, runID uuid.UUID) error {
	if r.appClient() == nil {
		return nil
	}
	return r.store.WithRunCheckLock(ctx, runID, func() error {
		return r.reopenLocked(ctx, runID)
	})
}

func (r *Reporter) reopenLocked(ctx context.Context, runID uuid.UUID) error {
	app := r.appClient()
	if app == nil {
		return nil
	}
	link, err := r.store.GetGithubCheckRun(ctx, runID)
	switch {
	case errors.Is(err, store.ErrCheckRunNotFound):
		// No prior check: a fresh run (full rerun) or a non-reportable
		// cause. CreateCheck creates+links, or no-ops cleanly.
		if err := r.CreateCheck(ctx, runID); err != nil {
			return err
		}
	case err != nil:
		return err
	case link.Completed:
		// GitHub never cleanly re-opens a check run that already completed —
		// completed_at is set-once, so PATCHing it back to in_progress leaves
		// the PR showing the prior conclusion for the whole rerun (looks like
		// "only reports at the end"). Create a FRESH check run instead (clean
		// in_progress); CreateCheck re-links run→new check and resets the
		// completed flag to FALSE. The per-run lock serialises concurrent
		// job-reruns: the first recreates, the rest then see completed=FALSE
		// and take the reuse PATCH below — so no check run is orphaned.
		if err := r.CreateCheck(ctx, runID); err != nil {
			return err
		}
		r.log.Info("checks: reopened (new check run)", "run_id", runID)
	default:
		if err := app.UpdateCheckRun(ctx, link.InstallationID, ghscm.UpdateCheckRunInput{
			Owner:      link.Owner,
			Repo:       link.Repo,
			CheckRunID: link.CheckRunID,
			Status:     ghscm.CheckStatusInProgress,
			Output: &ghscm.CheckRunOutput{
				Title:   "Pipeline re-running",
				Summary: "A rerun is in progress — follow the run for details.",
			},
		}); err != nil {
			return fmt.Errorf("reopen check run: %w", err)
		}
		r.log.Info("checks: reopened",
			"run_id", runID, "check_run_id", link.CheckRunID)
	}

	// Self-heal the race: a very fast rerun can finish before this reopen
	// lands. completeCheckLocked re-reads the run — it no-ops while the
	// rerun is running, or closes the check if it already finished. Called
	// directly (not CompleteCheck) because we already hold the lock.
	return r.completeCheckLocked(ctx, runID, "")
}

// runTerminalStatus reports the run's current status and whether it is a
// terminal state. Used by ReopenCheck's self-heal.
func (r *Reporter) runTerminalStatus(ctx context.Context, runID uuid.UUID) (string, bool, error) {
	detail, err := r.store.GetRunDetail(ctx, runID, 0, nil)
	if err != nil {
		return "", false, fmt.Errorf("get run detail: %w", err)
	}
	switch detail.Status {
	case string(domain.StatusSuccess), string(domain.StatusFailed),
		string(domain.StatusCanceled), string(domain.StatusSkipped):
		return detail.Status, true, nil
	default:
		return detail.Status, false, nil
	}
}

// coverageSummaryLine renders the run's coverage rows as markdown
// for the check output. Empty when the run reported none. The
// percentage formula matches the agent's log line and the UI
// (100×covered/total, one decimal) — three surfaces, one number.
func (r *Reporter) coverageSummaryLine(ctx context.Context, runID uuid.UUID) string {
	rows, err := r.store.CoverageByRun(ctx, runID)
	if err != nil || len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("**Coverage**")
	for _, row := range rows {
		name := row.JobName
		if row.MatrixKey != "" {
			name += " [" + row.MatrixKey + "]"
		}
		if row.LinesTotal <= 0 {
			continue
		}
		pct := 100 * float64(row.LinesCovered) / float64(row.LinesTotal)
		fmt.Fprintf(&b, "\n- `%s`: %.1f%%", name, pct)
		if base := row.Baseline; base != nil && base.LinesTotal > 0 {
			delta := pct - 100*float64(base.LinesCovered)/float64(base.LinesTotal)
			switch {
			case delta >= 0.05:
				fmt.Fprintf(&b, " (+%.1fpp vs main)", delta)
			case delta <= -0.05:
				fmt.Fprintf(&b, " (−%.1fpp vs main)", -delta)
			default:
				b.WriteString(" (±0.0pp vs main)")
			}
		}
	}
	return b.String()
}

// runContext is the shape reporter needs: triggering material URL,
// head SHA, pipeline name, counter, branch. Separated into a struct
// so resolveRunContext can return nil cleanly when the run shouldn't
// report.
type runContext struct {
	owner, repo  string
	headSHA      string
	pipelineName string
	branch       string
	counter      int64
}

func (r *Reporter) resolveRunContext(ctx context.Context, runID uuid.UUID) (*runContext, error) {
	detail, err := r.store.GetRunDetail(ctx, runID, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("get run detail: %w", err)
	}
	// Only report for webhook-driven runs. Manual/upstream runs
	// don't have a specific head SHA to report against.
	switch detail.Cause {
	case string(domain.CauseWebhook), "pull_request":
	default:
		return nil, nil
	}
	if len(detail.Revisions) == 0 {
		return nil, nil
	}
	var revisions map[string]struct {
		Revision string `json:"revision"`
		Branch   string `json:"branch"`
	}
	if err := json.Unmarshal(detail.Revisions, &revisions); err != nil {
		return nil, fmt.Errorf("decode revisions: %w", err)
	}
	if len(revisions) == 0 {
		return nil, nil
	}

	// Pick the first material that has a revision (usually the only
	// one on a webhook-driven run). We also need its URL — query the
	// store for the materials so we can resolve owner/repo.
	mats, err := r.store.ListPipelineMaterials(ctx, detail.PipelineID)
	if err != nil {
		return nil, fmt.Errorf("list materials: %w", err)
	}

	var triggeringID uuid.UUID
	var headSHA, branch string
	for id, rev := range revisions {
		if rev.Revision == "" {
			continue
		}
		u, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		triggeringID = u
		headSHA = rev.Revision
		branch = rev.Branch
		break
	}
	if triggeringID == uuid.Nil {
		return nil, nil
	}

	// For PR runs, head SHA from cause_detail is authoritative (the
	// PR head commit, not the material's internal "revision" field).
	if detail.Cause == "pull_request" && len(detail.CauseDetail) > 0 {
		var cd map[string]any
		if err := json.Unmarshal(detail.CauseDetail, &cd); err == nil {
			if sha, ok := cd["pr_head_sha"].(string); ok && sha != "" {
				headSHA = sha
			}
		}
	}

	var repoURL string
	for _, m := range mats {
		if m.ID == triggeringID {
			var cfg domain.GitMaterial
			if err := json.Unmarshal(m.Config, &cfg); err == nil {
				repoURL = cfg.URL
			}
			break
		}
	}
	if repoURL == "" {
		return nil, nil
	}
	if !isGitHubHost(repoURL) {
		// ParseRepoURL also accepts gitlab/bitbucket shaped URLs;
		// Checks API is github-specific so skip anything else.
		return nil, nil
	}
	owner, repo, err := ghscm.ParseRepoURL(repoURL)
	if err != nil {
		return nil, nil
	}

	return &runContext{
		owner:        owner,
		repo:         repo,
		headSHA:      headSHA,
		pipelineName: detail.PipelineName,
		branch:       branch,
		counter:      detail.Counter,
	}, nil
}

func (r *Reporter) detailsURL(runID uuid.UUID) string {
	return r.publicBase + "/runs/" + runID.String()
}

// isGitHubHost returns true for URLs whose host is github.com. We
// keep the check narrow — GitHub Enterprise host validation belongs
// at a higher level where the operator configures the enterprise
// APIBase, not here.
func isGitHubHost(repoURL string) bool {
	s := strings.ToLower(repoURL)
	switch {
	case strings.HasPrefix(s, "https://github.com/"),
		strings.HasPrefix(s, "http://github.com/"),
		strings.HasPrefix(s, "git@github.com:"):
		return true
	}
	return false
}

// conclusionFor maps gocdnext's terminal states onto GitHub's
// check conclusion enum. Anything unexpected falls back to
// "neutral" so the check still closes out.
func conclusionFor(status string) ghscm.CheckRunConclusion {
	switch status {
	case string(domain.StatusSuccess):
		return ghscm.CheckConclusionSuccess
	case string(domain.StatusFailed):
		return ghscm.CheckConclusionFailure
	case string(domain.StatusCanceled):
		return ghscm.CheckConclusionCancelled
	case string(domain.StatusSkipped):
		return ghscm.CheckConclusionNeutral
	default:
		return ghscm.CheckConclusionNeutral
	}
}
