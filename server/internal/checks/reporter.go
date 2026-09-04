// Package checks reports run state transitions back to GitHub as
// Check Runs. Activated only when a GitHub App is configured AND the
// run was triggered by a webhook (push, pull_request, or merge_group)
// on a repo where the App is installed. Manual / upstream runs skip silently.
package checks

import (
	"context"
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

func (r *Reporter) appClientForLink(link store.GithubCheckRun) *ghscm.AppClient {
	if r == nil || r.vcs == nil {
		return nil
	}
	if link.AppID != nil {
		return r.vcs.GitHubAppByID(*link.AppID)
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

// derefInt64 renders a nullable check-run id for logs (0 = commit_status mode).
func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// CompleteCheck is the synchronous version of ReportRunCompleted.
// Returns nil when we have no check record for the run (feature
// disabled, or create-side skipped). Serialized per run (advisory lock)
// with ReopenCheck so a stale completion can't land between a concurrent
// reopen's status read and its PATCH.
func (r *Reporter) CompleteCheck(ctx context.Context, runID uuid.UUID, status string) error {
	if r == nil {
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
	suppress, err := r.suppressMergeGroupDestroyedCancellation(ctx, runID, status, link)
	if err != nil {
		return err
	}
	if suppress {
		return nil
	}
	app := r.appClientForLink(link)
	if app == nil {
		if rc, rerr := r.resolveRunContext(ctx, runID); rerr == nil && rc != nil {
			if err := mergeGroupAppDisabledError(rc); err != nil {
				return err
			}
		}
		return nil
	}

	mode := normalizeMode(link.ReportingMode)
	conclusion := conclusionFor(status)
	// Coverage + security enrichment via the shared composer: the check
	// summary carries per-series coverage deltas and the security posture
	// (open + new vs base) — the numbers a PR reviewer wants without leaving
	// GitHub. Best-effort: a lookup failure degrades to the plain summary.
	title, summary := r.composeCheckOutput(ctx, runID, status, true)

	// Complete the Check Run (both/check_run). Skipped cleanly in commit_status
	// mode, where the row exists as identity but check_run_id is NULL.
	if postsCheckRun(mode) && link.CheckRunID != nil {
		if err := app.UpdateCheckRun(ctx, link.InstallationID, ghscm.UpdateCheckRunInput{
			Owner:      link.Owner,
			Repo:       link.Repo,
			CheckRunID: *link.CheckRunID,
			Status:     ghscm.CheckStatusCompleted,
			Conclusion: conclusion,
			Output: &ghscm.CheckRunOutput{
				Title:   title,
				Summary: summary,
			},
		}); err != nil {
			return fmt.Errorf("patch check run: %w", err)
		}
	}
	// Mirror the terminal state onto the commit status (both/commit_status),
	// using the PERSISTED identity + context (never re-derived — the material
	// may have changed). In commit_status mode the status is the ONLY channel,
	// so a failed post hard-fails here — returning BEFORE MarkGithubCheckRun
	// Completed below, so `completed` stays false and a later refresh retries
	// rather than the run reading "completed" with a stuck/absent status.
	desc := ""
	if rc, rerr := r.resolveRunContext(ctx, runID); rerr == nil && rc != nil {
		desc = fmt.Sprintf("Run #%d on %s", rc.counter, rc.branch)
	}
	if err := r.postStatusForMode(ctx, app, mode, link.InstallationID, ghscm.CreateStatusInput{
		Owner:       link.Owner,
		Repo:        link.Repo,
		SHA:         link.HeadSHA,
		State:       statusStateFor(status),
		Context:     link.StatusContext,
		TargetURL:   r.detailsURL(runID),
		Description: desc,
	}, runID); err != nil {
		return err
	}
	r.log.Info("checks: updated",
		"run_id", runID, "mode", mode, "check_run_id", derefInt64(link.CheckRunID),
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

// RefreshSecuritySummary re-PATCHes a run's check output after SARIF ingestion
// lands (it's async, so a run whose scanner is the last job can complete + close
// the check before findings exist). Best-effort + self-healing: GitHub converges
// to the security line once the scan reconciles. Takes the per-run check lock
// ONCE and dispatches to locked-variant helpers — never the public lock-taking
// methods (re-entry would deadlock on a second pooled connection).
func (r *Reporter) RefreshSecuritySummary(ctx context.Context, runID uuid.UUID) error {
	if r == nil {
		return nil
	}
	return r.store.WithRunCheckLock(ctx, runID, func() error {
		return r.refreshSecurityLocked(ctx, runID)
	})
}

func (r *Reporter) refreshSecurityLocked(ctx context.Context, runID uuid.UUID) error {
	link, err := r.store.GetGithubCheckRun(ctx, runID)
	if errors.Is(err, store.ErrCheckRunNotFound) {
		return nil // no check for this run — nothing to refresh
	}
	if err != nil {
		return err
	}
	current, terminal, err := r.runTerminalStatus(ctx, runID)
	if err != nil {
		return err
	}
	// Terminal but not yet finalized in our DB → go through the shared complete
	// path (it composes the same output, PATCHes completed+conclusion, AND marks
	// github_check_runs.completed) so the run→check link stays consistent and a
	// later rerun recreates rather than reuses. We already hold the lock.
	if terminal && !link.Completed {
		return r.completeCheckLocked(ctx, runID, current)
	}
	// commit_status mode has no Check Run to enrich — the security line lives
	// only on the rich check output. The terminal convergence above already
	// posted the commit status; nothing else to refresh.
	if link.CheckRunID == nil {
		return nil
	}
	app := r.appClientForLink(link)
	if app == nil {
		return nil
	}
	// Otherwise re-PATCH output, reasserting (not deriving) the current state:
	// terminal+completed → re-send status=completed + conclusion + output;
	// still running → in_progress + output, no conclusion.
	title, summary := r.composeCheckOutput(ctx, runID, current, terminal)
	in := ghscm.UpdateCheckRunInput{
		Owner:      link.Owner,
		Repo:       link.Repo,
		CheckRunID: *link.CheckRunID,
		Output:     &ghscm.CheckRunOutput{Title: title, Summary: summary},
	}
	if terminal {
		in.Status = ghscm.CheckStatusCompleted
		in.Conclusion = conclusionFor(current)
	} else {
		in.Status = ghscm.CheckStatusInProgress
	}
	if err := app.UpdateCheckRun(ctx, link.InstallationID, in); err != nil {
		return fmt.Errorf("patch check run (security refresh): %w", err)
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
	if r == nil {
		return nil
	}
	return r.store.WithRunCheckLock(ctx, runID, func() error {
		return r.reopenLocked(ctx, runID)
	})
}

func (r *Reporter) reopenLocked(ctx context.Context, runID uuid.UUID) error {
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
		if err := r.createCheck(ctx, runID, r.appClientForLink(link), nil); err != nil {
			return err
		}
		r.log.Info("checks: reopened (new check run)", "run_id", runID)
	default:
		app := r.appClientForLink(link)
		if app == nil {
			return nil
		}
		// Reopen in the run's PERSISTED mode (link.ReportingMode), never the
		// project's current setting — a mid-run flip must not change how an
		// in-flight run reports.
		mode := normalizeMode(link.ReportingMode)
		if postsCheckRun(mode) && link.CheckRunID != nil {
			if err := app.UpdateCheckRun(ctx, link.InstallationID, ghscm.UpdateCheckRunInput{
				Owner:      link.Owner,
				Repo:       link.Repo,
				CheckRunID: *link.CheckRunID,
				Status:     ghscm.CheckStatusInProgress,
				Output: &ghscm.CheckRunOutput{
					Title:   "Pipeline re-running",
					Summary: "A rerun is in progress — follow the run for details.",
				},
			}); err != nil {
				return fmt.Errorf("reopen check run: %w", err)
			}
		}
		// Reset the commit status to pending for the rerun — persisted identity +
		// context. Best-effort in both/check_run; hard-fails in commit_status.
		desc := ""
		if rc, rerr := r.resolveRunContext(ctx, runID); rerr == nil && rc != nil {
			desc = fmt.Sprintf("Run #%d on %s", rc.counter, rc.branch)
		}
		if err := r.postStatusForMode(ctx, app, mode, link.InstallationID, ghscm.CreateStatusInput{
			Owner:       link.Owner,
			Repo:        link.Repo,
			SHA:         link.HeadSHA,
			State:       "pending",
			Context:     link.StatusContext,
			TargetURL:   r.detailsURL(runID),
			Description: desc,
		}, runID); err != nil {
			return err
		}
		r.log.Info("checks: reopened",
			"run_id", runID, "mode", mode, "check_run_id", derefInt64(link.CheckRunID))
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

// securitySummaryLine renders the check-run security line from the run's
// findings + base-branch delta. Identity counts (not SARIF occurrences); empty
// when the run had no reconciled scan (non-fatal — a lookup failure degrades to
// no line, never blocks the check). Segments: open severities, then `· N
// accepted` (acknowledged risk, never folded into open), `· N new vs base` only
// when a comparable base exists, `· N series without base` when some scanner
// series have no base to diff against.
func (r *Reporter) securitySummaryLine(ctx context.Context, runID uuid.UUID) string {
	sec, err := r.store.RunSecuritySummary(ctx, runID)
	if err != nil || !sec.HasScans {
		return ""
	}
	var openParts []string
	for _, s := range []struct {
		n     int64
		label string
	}{{sec.Critical, "critical"}, {sec.High, "high"}, {sec.Medium, "medium"}, {sec.Low, "low"}} {
		if s.n > 0 {
			openParts = append(openParts, fmt.Sprintf("%d %s", s.n, s.label))
		}
	}
	var b strings.Builder
	b.WriteString("**Security** — ")
	if len(openParts) == 0 {
		b.WriteString("0 open")
	} else {
		b.WriteString(strings.Join(openParts, ", "))
		b.WriteString(" open")
	}
	if sec.Accepted > 0 {
		fmt.Fprintf(&b, " · %d accepted", sec.Accepted)
	}
	if sec.DeltaAvailable {
		fmt.Fprintf(&b, " · %d new vs base", len(sec.NewInChange))
	}
	if sec.UnbaselinedSeries > 0 {
		fmt.Fprintf(&b, " · %d series without base", sec.UnbaselinedSeries)
	}
	return b.String()
}

// composeCheckOutput builds the check run's title + summary, shared by the
// completion path and the post-ingest security refresh so the two can never
// drift. Coverage + security lines are appended best-effort.
func (r *Reporter) composeCheckOutput(ctx context.Context, runID uuid.UUID, status string, terminal bool) (title, summary string) {
	if terminal {
		title = "Pipeline " + status
		summary = fmt.Sprintf("gocdnext run finished with status=%s.", status)
	} else {
		title = "Pipeline running"
		summary = "gocdnext run in progress."
	}
	if cov := r.coverageSummaryLine(ctx, runID); cov != "" {
		summary += "\n\n" + cov
	}
	if sec := r.securitySummaryLine(ctx, runID); sec != "" {
		summary += "\n\n" + sec
	}
	return title, summary
}

// Reporting-mode predicates. The effective mode is persisted per-run on the
// github_check_runs row (never re-derived mid-run), so complete/reopen read it
// back from the link. normalizeMode treats an empty value (a link pre-dating
// the column) as the backward-compatible default.
func normalizeMode(mode string) string {
	if mode == "" {
		return store.CheckReportingBoth
	}
	return mode
}

func postsCheckRun(mode string) bool {
	m := normalizeMode(mode)
	return m == store.CheckReportingBoth || m == store.CheckReportingCheckRun
}

func postsCommitStatus(mode string) bool {
	m := normalizeMode(mode)
	return m == store.CheckReportingBoth || m == store.CheckReportingCommitStatus
}

func (r *Reporter) detailsURL(runID uuid.UUID) string {
	return r.publicBase + "/runs/" + runID.String()
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

// statusContext is the commit-status context — deliberately DISTINCT from the
// check run NAME ("gocdnext / <pipeline>") so the two entries don't collide in
// the PR checks list, and parallel to the ci/<tool>/<pipeline> convention. The
// project slug qualifies the pipeline name so two projects watching the SAME
// repo with the same pipeline name don't overwrite each other's status (a
// status is keyed by repo+sha+context). GitHub compares contexts
// case-insensitively, so pipeline names differing only by case still collide —
// keep them repo-unique.
func statusContext(projectSlug, pipelineName string) string {
	return "ci/gocdnext/" + projectSlug + "/" + pipelineName
}

// StatusContext exposes the commit-status context string for a (project,
// pipeline) so other subsystems — notably the required-checks ruleset writer —
// require the exact same context gocdnext posts. Single source of truth: a
// required check whose name doesn't match this byte-for-byte would leave the PR
// waiting on a check that never arrives.
func StatusContext(projectSlug, pipelineName string) string {
	return statusContext(projectSlug, pipelineName)
}

// statusStateFor maps a run status to a GitHub commit-status state
// (pending|success|failure|error). Skipped → success (neutral/non-blocking,
// mirroring the check run's conclusion); a non-terminal status → pending.
func statusStateFor(status string) string {
	switch status {
	case string(domain.StatusSuccess), string(domain.StatusSkipped):
		return "success"
	case string(domain.StatusFailed):
		return "failure"
	case string(domain.StatusCanceled):
		return "error"
	default:
		return "pending"
	}
}

// postCommitStatus mirrors the run state as a commit status whose row links
// STRAIGHT to the run (target_url) — the entry teams migrating from
// Woodpecker/GoCD expect, alongside the richer check run. It skips cleanly when
// Context is empty (a link created before this feature). Best-effort: a missing
// "Commit statuses: write" App permission (or any error) is logged and
// swallowed; the check run is authoritative, the status is additive.
//
// Callers pass the identity (owner/repo/sha/context) from the PERSISTED link on
// terminal/reopen — never re-derived from a material that may have changed —
// so the terminal update always lands on the same status the pending post
// created, and can never leave it stuck in `pending`.
func (r *Reporter) postCommitStatus(ctx context.Context, app *ghscm.AppClient, installationID int64, in ghscm.CreateStatusInput, runID uuid.UUID) error {
	if in.Context == "" {
		return nil
	}
	if err := app.CreateStatus(ctx, installationID, in); err != nil {
		r.log.Warn("checks: commit status not posted (App needs 'Commit statuses: write'?)",
			"run_id", runID, "err", err)
		return err
	}
	return nil
}

// postStatusForMode posts the commit status and, in commit_status mode — where
// it is the ONLY channel — propagates a post failure as a hard error so the
// caller does NOT mark the run completed on a status that never landed (e.g. a
// 403 for a missing "Commit statuses: write" permission would otherwise leave
// GitHub stuck at pending while the link reads completed). In both/check_run
// the Check Run is authoritative, so a status failure stays best-effort
// (logged + swallowed). No-op when the mode doesn't post a status.
func (r *Reporter) postStatusForMode(ctx context.Context, app *ghscm.AppClient, mode string, installationID int64, in ghscm.CreateStatusInput, runID uuid.UUID) error {
	if !postsCommitStatus(mode) {
		return nil
	}
	err := r.postCommitStatus(ctx, app, installationID, in, runID)
	if err != nil && mode == store.CheckReportingCommitStatus {
		return fmt.Errorf("commit status is the only reporting channel in commit_status mode: %w", err)
	}
	return nil
}
