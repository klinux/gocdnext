package checks

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// CreateCheck is the synchronous version of ReportRunCreated —
// callable from tests and from any caller that wants to know whether
// the check was created. Returns nil when the run shouldn't produce
// a check (manual/upstream cause, non-GitHub repo, App not
// installed) so callers can't trivially tell "created" from
// "skipped"; check logs for that. merge_group declines are errors because
// a missing check can strand GitHub's merge queue.
func (r *Reporter) CreateCheck(ctx context.Context, runID uuid.UUID) error {
	if r == nil {
		return nil
	}
	return r.createCheck(ctx, runID, r.appClient(), nil)
}

// CreateCheckWithApp is the synchronous create path for App-delivered events
// whose HMAC has already identified the exact GitHub App. It avoids using the
// registry's primary App in multi-App deployments.
func (r *Reporter) CreateCheckWithApp(ctx context.Context, runID uuid.UUID, app *ghscm.AppClient) error {
	if r == nil {
		return nil
	}
	return r.createCheck(ctx, runID, app, nil)
}

// CreateCheckWithAppInstallation is CreateCheckWithApp with an already-verified
// installation id. merge_group webhooks validate the payload's installation
// before enqueuing, so they can reuse it here instead of doing a second API
// lookup in the synchronous request path.
func (r *Reporter) CreateCheckWithAppInstallation(ctx context.Context, runID uuid.UUID, app *ghscm.AppClient, installationID int64) error {
	if r == nil {
		return nil
	}
	return r.createCheck(ctx, runID, app, &installationID)
}

func (r *Reporter) createCheck(ctx context.Context, runID uuid.UUID, app *ghscm.AppClient, installationIDOverride *int64) error {
	ctxInfo, err := r.resolveRunContext(ctx, runID)
	if err != nil {
		return err
	}
	if ctxInfo == nil {
		return nil // non-reportable cause, non-GitHub repo, etc.
	}
	if app == nil {
		if err := mergeGroupAppDisabledError(ctxInfo); err != nil {
			return err
		}
		return nil
	}

	// Effective mode: STICKY to the mode this run started in — a mid-run
	// settings flip must not change how an in-flight run reports. reopen's
	// recreate path routes through here for the SAME run, so the persisted
	// row's mode wins over the project's current setting. Fresh run → no row
	// yet → the project's current mode.
	mode := normalizeMode(ctxInfo.reportingMode)
	if existing, gerr := r.store.GetGithubCheckRun(ctx, runID); gerr == nil {
		mode = normalizeMode(existing.ReportingMode)
		existingApp := app
		if existing.AppID != nil && app != nil && app.AppID() == *existing.AppID {
			existingApp = app
		} else if linkedApp := r.appClientForLink(existing); linkedApp != nil {
			existingApp = linkedApp
		} else if existing.AppID != nil {
			existingApp = nil
		}
		if existingApp == nil {
			if err := mergeGroupAppDisabledError(ctxInfo); err != nil {
				return err
			}
			return nil
		}
		if !existing.Completed {
			return r.postStatusForMode(ctx, existingApp, mode, existing.InstallationID, ghscm.CreateStatusInput{
				Owner:       existing.Owner,
				Repo:        existing.Repo,
				SHA:         existing.HeadSHA,
				State:       "pending",
				Context:     existing.StatusContext,
				TargetURL:   r.detailsURL(runID),
				Description: fmt.Sprintf("Run #%d on %s", ctxInfo.counter, ctxInfo.branch),
			}, runID)
		}
	} else if !errors.Is(gerr, store.ErrCheckRunNotFound) {
		return gerr
	}

	// The installation is needed in EVERY mode — the commit status is posted
	// through the same App installation as the check run.
	var installationID int64
	if installationIDOverride != nil && *installationIDOverride > 0 {
		installationID = *installationIDOverride
	} else {
		var err error
		installationID, err = app.InstallationID(ctx, ctxInfo.owner, ctxInfo.repo)
		if errors.Is(err, ghscm.ErrNoInstallation) {
			if err := mergeGroupNoInstallationError(ctxInfo); err != nil {
				return err
			}
			r.log.Info("checks: app not installed, skipping",
				"run_id", runID, "repo", ctxInfo.owner+"/"+ctxInfo.repo)
			return nil
		}
		if err != nil {
			return fmt.Errorf("installation lookup: %w", err)
		}
	}

	// Create the rich Check Run unless the mode is commit_status. When skipped,
	// the row still persists (identity) with a NULL check_run_id.
	var checkRunID *int64
	if postsCheckRun(mode) {
		created, cerr := app.CreateCheckRun(ctx, installationID, ghscm.CreateCheckRunInput{
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
		if cerr != nil {
			return fmt.Errorf("create check run: %w", cerr)
		}
		id := created.ID
		checkRunID = &id
	}

	sc := statusContext(ctxInfo.projectSlug, ctxInfo.pipelineName)
	appID := app.AppID()
	if err := r.store.UpsertGithubCheckRun(ctx, store.UpsertGithubCheckRunInput{
		RunID:          runID,
		AppID:          &appID,
		InstallationID: installationID,
		CheckRunID:     checkRunID,
		Owner:          ctxInfo.owner,
		Repo:           ctxInfo.repo,
		HeadSHA:        ctxInfo.headSHA,
		StatusContext:  sc,
		ReportingMode:  mode,
	}); err != nil {
		return fmt.Errorf("persist check link: %w", err)
	}

	// Mirror as a commit status (straight-to-run link) unless the mode is
	// check_run. Best-effort in both/check_run; hard-fails in commit_status
	// (the only channel there).
	if err := r.postStatusForMode(ctx, app, mode, installationID, ghscm.CreateStatusInput{
		Owner:       ctxInfo.owner,
		Repo:        ctxInfo.repo,
		SHA:         ctxInfo.headSHA,
		State:       "pending",
		Context:     sc,
		TargetURL:   r.detailsURL(runID),
		Description: fmt.Sprintf("Run #%d on %s", ctxInfo.counter, ctxInfo.branch),
	}, runID); err != nil {
		return err
	}

	r.log.Info("checks: created",
		"run_id", runID, "mode", mode, "check_run_id", derefInt64(checkRunID),
		"repo", ctxInfo.owner+"/"+ctxInfo.repo, "head_sha", ctxInfo.headSHA)
	return nil
}
