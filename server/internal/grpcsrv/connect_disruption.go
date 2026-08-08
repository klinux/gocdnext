package grpcsrv

import (
	"context"
	"time"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// afterJobCompletion runs the post-completion side-effects shared by the
// normal terminal path (handleJobResult) and the DISRUPTED-terminal path
// (handleDisruptedResult): cold-archive enqueue, security-findings ingest,
// deploy-revision finalise, run-completed service teardown, and the
// scheduler wake. Extracted so a disrupted job that terminalises (capped /
// unsafe / cancel-won) gets byte-for-byte the same tail as an ordinary
// terminal result — no silently-missing deploy finalise or leaked service
// pods. Session bookkeeping (ClearAssignment/DecRunning) stays with each
// caller because the ordering differs (a requeue must clear BEFORE notify).
func (a *AgentService) afterJobCompletion(
	ctx context.Context,
	log logger,
	batcher *logBatcher,
	comp store.JobCompletion,
	expectedAttempt int32,
) {
	// Cold-archive enqueue. The archiver runs async — the worker
	// pool will read the job's log_lines, gzip + upload, then drop
	// the rows. Per-project override is resolved live so an admin
	// toggle takes effect on the next terminating job without a
	// service restart.
	a.maybeEnqueueArchive(ctx, log, batcher, comp.JobRunID)

	// Security findings (#71): parse this job's *.sarif artifacts and reconcile
	// the findings table. Async + best-effort — never affects the job result.
	a.ingestSecurityFindings(comp.JobRunID, expectedAttempt)

	// Deployment tracking (#39): if this job carried a `deploy:`
	// marker, the dispatch path opened an in_progress revision.
	// Finalise it to match the job's terminal outcome. Best-effort
	// (a tracking-write failure must not affect the run cascade) and
	// idempotent: 0 rows means the job had no deploy: block (the
	// common case), and the status='in_progress' guard makes a
	// re-delivered terminal result a no-op.
	//
	// EFFECTIVE status (#207): a canceled deploy job records a 'canceled' revision
	// (never becomes current, stays in history, excluded from DORA), not 'failed'.
	deployStatus := store.DeployStatusFailed
	switch comp.JobStatus {
	case string(domain.StatusSuccess):
		deployStatus = store.DeployStatusSuccess
	case string(domain.StatusCanceled):
		deployStatus = store.DeployStatusCanceled
	}
	if _, err := a.store.FinalizeDeploymentRevision(ctx, comp.JobRunID, expectedAttempt, deployStatus); err != nil {
		log.Warn("deploy tracking: finalize revision", "job_id", comp.JobRunID, "err", err)
	}

	if comp.RunCompleted {
		a.checksReporter.ReportRunCompleted(ctx, comp.RunID, comp.RunStatus)
		// Run-scoped service teardown, broadcast to every agent that ran a
		// job of this run (engine types may differ across jobs). Idempotent
		// per-engine. Runs in a goroutine with a fresh Background context so
		// a stream drop right after the JobResult doesn't kill the cleanup.
		runID := comp.RunID
		serviceGen := comp.ServiceGeneration
		go func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			hasServices, hsErr := a.store.RunHasServices(cleanupCtx, runID)
			if hsErr != nil {
				log.Warn("cleanup run services: has-services check failed; dispatching anyway",
					"run_id", runID, "err", hsErr)
				hasServices = true // fail-open: better one extra List than leak
			}
			if hasServices {
				a.dispatchRunServiceCleanup(cleanupCtx, log, runID, serviceGen)
			}
		}()
	}

	// Wake the scheduler on every non-terminal-run completion so same-stage
	// `needs:` siblings don't wait a full periodic tick. NOTIFY is cheap and
	// the dispatch handler no-ops when there's no eligible work.
	if !comp.RunCompleted {
		if err := a.store.NotifyRunQueued(ctx, comp.RunID); err != nil {
			log.Warn("agent result: notify run_queued", "err", err)
		}
	}
}

// handleDisruptedResult processes a JobResult the agent reported as
// RUN_STATUS_DISRUPTED (task pod preempted / evicted / node-reclaimed). It is
// dispatched from handleJobResult BEFORE mapStatus / artifact / output
// validation — a disrupted job never uploaded artifacts and its non-zero exit
// is a platform teardown, not a real failure, so none of that validation
// applies. The requeue-vs-terminal decision + DB action live in
// store.HandleDisruptedJob; here we translate the verdict into session
// bookkeeping, the outcome metric, and the re-dispatch / terminal tail.
func (a *AgentService) handleDisruptedResult(
	ctx context.Context,
	log logger,
	sess *Session,
	batcher *logBatcher,
	jobID uuid.UUID,
	expectedAttempt int32,
	r *gocdnextv1.JobResult,
) {
	res, err := a.store.HandleDisruptedJob(ctx, store.HandleDisruptedInput{
		JobRunID:        jobID,
		ExpectedAgentID: sess.AgentID,
		ExpectedAttempt: expectedAttempt,
		MaxAttempts:     a.registerFenceMaxAttempts, // shared fleet cap (reaper / fence / disruption)
		Reason:          r.GetError(),
	})
	if err != nil {
		log.Warn("agent result: handle disrupted", "err", err, "job_id", jobID)
		return
	}
	if res.Outcome == store.DisruptedSkipped {
		// Already terminal, or the (agent, attempt) snapshot moved (a
		// concurrent reaper/rerun owns the row). Mirror the normal handler's
		// stale-completion path: touch nothing (no session decrement — the
		// owning path already accounted for it).
		log.Debug("agent result: disrupted job already terminal or snapshot stale", "job_id", jobID)
		return
	}

	// Free THIS session's assignment + capacity AFTER the store decided and
	// (for a requeue) BEFORE NotifyRunQueued: otherwise the scheduler's LISTEN
	// wake could redispatch the requeued job onto the just-freed slot while
	// the old (jobID → attempt) entry is still recorded, failing
	// RecordAssignmentCAS. DecRunning goes via sess directly (not
	// a.sessions.Release) to pin the decrement to the session that accepted
	// the assignment — same reason as the normal terminal path.
	sess.ClearAssignment(jobID)
	sess.DecRunning()
	metrics.JobsRunning.Dec()
	metrics.JobsDisrupted.WithLabelValues(string(res.Outcome)).Inc()

	if res.Outcome == store.DisruptedRequeued {
		// The agent is healthy (only the task pod died); the scheduler may
		// reassign to this same session or another — the cleared assignment
		// above makes either safe.
		if err := a.store.NotifyRunQueued(ctx, res.RunID); err != nil {
			log.Warn("agent result: disrupted requeue notify", "err", err, "run_id", res.RunID)
		}
		log.Info("agent job disrupted → requeued", "job_id", jobID, "run_id", res.RunID)
		return
	}

	// Terminal (failed_capped / failed_unsafe_target / canceled): the
	// stage/run cascade already ran inside CompleteJob. Fire the same
	// post-completion tail the normal path does + the duration histogram.
	comp := res.Completion // non-nil for terminal outcomes
	if comp.StartedAt != nil && comp.FinishedAt != nil {
		if d := comp.FinishedAt.Sub(*comp.StartedAt).Seconds(); d >= 0 {
			metrics.JobDurationSeconds.
				WithLabelValues(metrics.JobStatusLabel(comp.JobStatus)).
				Observe(d)
		}
	}
	log.Info("agent job disrupted → terminal",
		"job_id", jobID, "run_id", comp.RunID, "outcome", res.Outcome,
		"status", comp.JobStatus, "run_done", comp.RunCompleted)

	a.afterJobCompletion(ctx, log, batcher, *comp, expectedAttempt)
}
