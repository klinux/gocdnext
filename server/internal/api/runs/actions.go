package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	gh "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Cancel handles POST /api/v1/runs/{id}/cancel.
// Response:
//
//	202 Accepted   — the run was active and is now canceled
//	404 Not Found  — unknown run id
//	409 Conflict   — run already in a terminal status
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	res, err := h.store.CancelRun(r.Context(), runID)
	switch {
	case err == nil:
		// Fan CancelJob messages out to every agent running one of
		// the run's jobs. Without this push the DB is flipped to
		// canceled but the container keeps burning — see the
		// cancel-kills-container roadmap for the incident that
		// motivated this wiring.
		h.dispatchCancel(runID, res.RunningJobs)
		// Run-scoped service teardown: same logic as the
		// CompleteJob run-terminal cascade in connect.go.
		// Dispatched off the request context — if the client
		// disconnects after CancelRun committed the DB mutation,
		// the cleanup needs to keep running, otherwise the pods
		// leak. A short fixed timeout bounds the work.
		go h.dispatchCleanupServices(runID)
		audit.Emit(r.Context(), h.log, h.store,
			store.AuditActionRunCancel, "run", runID.String(),
			map[string]any{"signaled_jobs": len(res.RunningJobs)})
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":        runID.String(),
			"status":        "canceled",
			"signaled_jobs": len(res.RunningJobs),
		})
	case errors.Is(err, store.ErrRunNotFound):
		http.Error(w, "run not found", http.StatusNotFound)
	case errors.Is(err, store.ErrRunAlreadyTerminal):
		http.Error(w, "run already terminal", http.StatusConflict)
	default:
		h.log.Error("cancel run", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// CancelJob handles POST /api/v1/job_runs/{id}/cancel — single
// job-scoped cancel. Distinct from the run-scoped Cancel above:
// only the one job is touched, siblings (and the run as a whole)
// keep running. Operator-facing "cancel this one job, leave the
// others alone" lives here.
//
// Response:
//
//	202 Accepted   — the job was active and is now cancel-flagged
//	                 (queued → DB flipped; running → CancelJob
//	                 frame pushed to the owning agent)
//	404 Not Found  — unknown job_run id
//	409 Conflict   — job_run already terminal
//
// The response body returns the parent run_id and whether the
// cancel was signaled to an agent (operator UX hint: "we pushed
// the cancel; container may take a moment to stop").
func (h *Handler) CancelJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := chi.URLParam(r, "id")
	jobRunID, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "invalid job_run id", http.StatusBadRequest)
		return
	}

	res, err := h.store.CancelJobRun(r.Context(), jobRunID)
	switch {
	case err == nil:
		h.respondCancelJob(w, r, jobRunID, res)
	case errors.Is(err, store.ErrJobRunNotFound):
		http.Error(w, "job_run not found", http.StatusNotFound)
	case errors.Is(err, store.ErrJobRunTerminal):
		http.Error(w, "job_run already terminal", http.StatusConflict)
	default:
		h.log.Error("cancel job", "job_run_id", jobRunID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// respondCancelJob translates a CancelJobRunResult into the right
// HTTP envelope. The hard rule: never tell the client status=canceled
// when the cancel hasn't actually taken effect — `canceling` is the
// honest intermediate state until the agent's JobResult finalises
// the row.
//
//   - queued path (NeedsDispatch=false) → 202, status=canceled,
//     signaled=false. Cancel has landed in the DB; row is already
//     terminal because the agent never started the work.
//   - running path with successful dispatch → 202, status=canceling,
//     signaled=true. Container will stop on its own clock; the
//     agent's JobResult finalises the row to status=canceled.
//   - running path with failed dispatch → 202, status=canceling,
//     signaled=false. cancel_requested_at IS stamped in the DB by
//     CancelJobRun's tx, so the agent's next Register replays it
//     via the pending-cancel path in agent_service.Register; if
//     the agent stays offline, the reaper finalises via
//     ReclaimPendingCancelsForOfflineAgent.
//   - running path with no agent_id yet → 503 with retry hint.
//     This is the AssignJob window (status='running' but agent_id
//     transiently NULL); the cancel_requested_at predicate didn't
//     stamp because there's nothing to attribute it to yet.
//     Operator retries; AssignJob's atomic UPDATE will have
//     populated agent_id by then.
func (h *Handler) respondCancelJob(
	w http.ResponseWriter, r *http.Request, jobRunID uuid.UUID, res store.CancelJobRunResult,
) {
	if !res.NeedsDispatch {
		// Queued path: the cancel landed in the DB transactionally,
		// no gRPC frame was sent (and none was needed — the job
		// never reached an agent). Audit signaled=false matches
		// the response, and deferred=false because there's nothing
		// to replay later — this row is already terminal.
		h.emitCancelAudit(r, jobRunID, res, false /*signaled*/, false /*deferred*/)
		writeCancelAccepted(w, jobRunID, res, false /*signaled*/, "canceled")
		return
	}

	// Running path. The DB row is still 'running'; the cancel only
	// lands when the agent's JobResult arrives, which only happens
	// after we successfully push CancelJob down the session.
	if res.Dispatched == nil {
		// Transient: row is running but the agent's session ack
		// hasn't landed yet — cancel_requested_at was NOT stamped
		// (the stamp predicate requires agent_id NOT NULL), so
		// there's nothing to replay either. Tell the operator to
		// retry once AssignJob has populated agent_id; the reaper
		// won't reclaim a row without cancel_requested_at via the
		// pending-cancel path.
		h.log.Warn("cancel job: running row has no agent_id (session not yet acked)",
			"run_id", res.RunID, "job_run_id", jobRunID)
		h.emitCancelAudit(r, jobRunID, res, false /*signaled*/, false /*deferred*/)
		writeCancelDispatchFailed(w, jobRunID, res,
			"agent session not yet established; retry in a moment")
		return
	}

	if h.dispatcher == nil {
		h.log.Warn("cancel job: no gRPC dispatcher wired; container keeps running",
			"run_id", res.RunID, "job_run_id", jobRunID)
		h.emitCancelAudit(r, jobRunID, res, false /*signaled*/, false /*deferred*/)
		writeCancelDispatchFailed(w, jobRunID, res,
			"server has no cancel dispatcher wired")
		return
	}

	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_Cancel{
			Cancel: &gocdnextv1.CancelJob{
				RunId:  res.RunID.String(),
				JobId:  res.Dispatched.JobID.String(),
				Reason: "user canceled job",
			},
		},
	}
	if err := h.dispatcher.Dispatch(res.Dispatched.AgentID, msg); err != nil {
		// Dispatch failed: agent's session is in flux right now
		// (Revoke→Register race during a pod restart, momentary
		// network glitch, busy queue). The DB still shows running,
		// the container is still burning — BUT cancel_requested_at
		// is already stamped by CancelJobRun's tx. The new session,
		// when it lands, drains the pending cancel via the replay
		// path in agent_service.Register. If the agent never comes
		// back, the reaper finalises the row.
		//
		// Honesty contract (closes the v0.14 honesty windows even
		// further): respond 202 status="canceling" not 503. The
		// cancel intent IS persisted; the operator's click was not
		// in vain. signaled=false reflects "agent has not yet acked"
		// — same nuance as the queued path. Audit records the dispatch
		// attempt with deferred=true so the trail distinguishes
		// "landed through gRPC" from "landed via DB → replay path".
		h.log.Warn("cancel job: dispatch failed, deferred to replay path",
			"run_id", res.RunID, "job_run_id", jobRunID,
			"agent_id", res.Dispatched.AgentID, "err", err)
		h.emitCancelAudit(r, jobRunID, res, false /*signaled*/, true /*deferred*/)
		writeCancelAccepted(w, jobRunID, res, false, "canceling")
		return
	}
	h.emitCancelAudit(r, jobRunID, res, true /*signaled*/, false /*deferred*/)
	writeCancelAccepted(w, jobRunID, res, true, "canceling")
}

// emitCancelAudit records the cancel attempt in the audit log
// with the same semantics the response uses for `signaled`:
//   - signaled=true  → CancelJob frame landed on the agent's
//     gRPC stream this turn.
//   - signaled=false → frame didn't land — either no agent to
//     dispatch to (queued path), or dispatch failed and the
//     replay path will deliver it on the next Register, or the
//     reaper will finalise the row. `deferred` distinguishes
//     "no dispatch attempted (queued / no agent yet)" from
//     "dispatch attempted and rescued to the replay path" so
//     forensic queries on the audit table can tell the two
//     apart without joining the response log.
func (h *Handler) emitCancelAudit(
	r *http.Request, jobRunID uuid.UUID, res store.CancelJobRunResult,
	signaled bool, deferred bool,
) {
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionJobCancel, "job_run", jobRunID.String(),
		map[string]any{
			"run_id":   res.RunID.String(),
			"job_name": res.JobName,
			"signaled": signaled,
			"deferred": deferred,
		})
}

func writeCancelAccepted(
	w http.ResponseWriter, jobRunID uuid.UUID, res store.CancelJobRunResult,
	signaled bool, status string,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"job_run_id": jobRunID.String(),
		"run_id":     res.RunID.String(),
		"job_name":   res.JobName,
		"status":     status,
		"signaled":   signaled,
	})
}

func writeCancelDispatchFailed(
	w http.ResponseWriter, jobRunID uuid.UUID, res store.CancelJobRunResult, reason string,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"job_run_id": jobRunID.String(),
		"run_id":     res.RunID.String(),
		"job_name":   res.JobName,
		"status":     "dispatch_failed",
		"signaled":   false,
		"error":      reason,
	})
}

// dispatchCancel pushes a CancelJob frame per running job to the
// owning agent's session. Best-effort: a missing/busy session is
// logged and skipped — the job's container eventually exits on
// its own and the server reconciles via the resulting JobResult.
// The dispatcher stays optional so unit tests + "DB-only" mode
// don't need the gRPC machinery wired.
func (h *Handler) dispatchCancel(runID uuid.UUID, jobs []store.RunningJobRef) {
	if h.dispatcher == nil {
		if len(jobs) > 0 {
			h.log.Warn("cancel: no gRPC dispatcher wired; containers keep running",
				"run_id", runID, "running_jobs", len(jobs))
		}
		return
	}
	for _, ref := range jobs {
		msg := &gocdnextv1.ServerMessage{
			Kind: &gocdnextv1.ServerMessage_Cancel{
				Cancel: &gocdnextv1.CancelJob{
					RunId:  runID.String(),
					JobId:  ref.JobID.String(),
					Reason: "user canceled",
				},
			},
		}
		if err := h.dispatcher.Dispatch(ref.AgentID, msg); err != nil {
			h.log.Warn("cancel: dispatch failed",
				"run_id", runID, "job_id", ref.JobID, "agent_id", ref.AgentID, "err", err)
		}
	}
}

// dispatchCleanupServices broadcasts CleanupRunServices to the
// union of (agents that ran a job of this run) ∪ (all currently
// connected agents). The wider net matters because the k8s agent
// that originally created the pods may have disconnected before
// the cancel, but ANY other k8s agent in the cluster can do the
// label-selector delete — pods are cluster-scoped.
//
// Owns its own context: callers invoke this in a goroutine after
// the HTTP request completes, so the request context is already
// expired by the time we run. A bounded fresh context lets the
// store lookup + per-agent dispatch finish without depending on
// client liveness.
//
// Best-effort: per-agent dispatch failure (agent disconnected)
// is logged but doesn't escalate. When the target set is empty
// OR every dispatch fails, we surface a warn-level log so the
// operator can grep for "pods may leak".
func (h *Handler) dispatchCleanupServices(runID uuid.UUID) {
	if h.dispatcher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mirror the RunCompleted-cascade gate in connect.go: pipelines
	// without a `services:` block don't have pods to clean, so skip
	// the broadcast entirely. Saves ListAgentsForRun + a k8s List
	// per agent per cancel. Fail-open on error: better to do one
	// extra empty List than to leak.
	hasServices, hsErr := h.store.RunHasServices(ctx, runID)
	if hsErr != nil {
		h.log.Warn("cancel: has-services check failed; dispatching cleanup anyway",
			"run_id", runID, "err", hsErr)
		hasServices = true
	}
	if !hasServices {
		return
	}

	ranAgents, err := h.store.ListAgentsForRun(ctx, runID)
	if err != nil {
		h.log.Warn("cancel: list agents for cleanup failed; continuing with connected-agents only",
			"run_id", runID, "err", err)
	}
	// k8s-only filter mirrors connect.go's RunCompleted broadcast.
	connected := h.dispatcher.AllAgentIDs("kubernetes")

	seen := make(map[uuid.UUID]struct{}, len(ranAgents)+len(connected))
	targets := make([]uuid.UUID, 0, len(ranAgents)+len(connected))
	for _, id := range ranAgents {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		targets = append(targets, id)
	}
	for _, id := range connected {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		targets = append(targets, id)
	}
	if len(targets) == 0 {
		h.log.Warn("cancel: cleanup has no targets; pods may leak until manual cleanup",
			"run_id", runID)
		return
	}

	msg := &gocdnextv1.ServerMessage{
		Kind: &gocdnextv1.ServerMessage_CleanupRunServices{
			CleanupRunServices: &gocdnextv1.CleanupRunServices{
				RunId: runID.String(),
			},
		},
	}
	var ok int
	for _, id := range targets {
		if err := h.dispatcher.Dispatch(id, msg); err != nil {
			h.log.Debug("cancel: cleanup dispatch to agent failed",
				"run_id", runID, "agent_id", id, "err", err)
			continue
		}
		ok++
	}
	if ok == 0 {
		h.log.Warn("cancel: all cleanup dispatches failed; pods may leak until manual cleanup",
			"run_id", runID, "targets", len(targets))
	}
}

// rerunBody optional JSON body for Rerun. triggered_by lands on the
// new run row so the UI can show who asked. Omit it and the store
// synthesizes "rerun:<orig>" automatically.
type rerunBody struct {
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// Rerun handles POST /api/v1/runs/{id}/rerun.
// Response on success (202):
//
//	{ "run_id": "...", "counter": <int>, "rerun_of": "<orig>" }
//
// 422 when the original run's revisions can't be replayed (e.g.,
// modifications pruned, blank revisions JSON).
func (h *Handler) Rerun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	body, ok := decodeOptionalBody[rerunBody](w, r)
	if !ok {
		return
	}

	res, err := h.store.RerunRun(r.Context(), store.RerunRunInput{
		RunID:       runID,
		TriggeredBy: body.TriggeredBy,
	})
	switch {
	case err == nil:
		audit.Emit(r.Context(), h.log, h.store,
			store.AuditActionRunRerun, "run", res.RunID.String(),
			map[string]any{"rerun_of": runID.String(), "counter": res.Counter})
		// Re-open the GitHub check for the NEW run so a PR shows the
		// rerun in progress instead of the prior (failed) conclusion.
		// No-op unless the run is webhook/PR-driven on a GitHub repo.
		if h.checks != nil {
			h.checks.ReportRunReopened(r.Context(), res.RunID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":   res.RunID.String(),
			"counter":  res.Counter,
			"rerun_of": runID.String(),
		})
	case errors.Is(err, store.ErrRunNotFound):
		http.Error(w, "run not found", http.StatusNotFound)
	case errors.Is(err, store.ErrNoModificationForPipeline),
		errors.Is(err, store.ErrRunRevisionsMissing):
		http.Error(w, "cannot replay this run: source revision is no longer available", http.StatusUnprocessableEntity)
	default:
		h.log.Error("rerun run", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// RerunJob handles POST /api/v1/job_runs/{id}/rerun — re-executes a
// single terminal job inside its existing run. Returns:
//
//	202 Accepted → { job_run_id, run_id, attempt }
//	404          → unknown job_run id
//	409          → job still active (queued/running); cancel first
func (h *Handler) RerunJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := chi.URLParam(r, "id")
	jobRunID, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "invalid job_run id", http.StatusBadRequest)
		return
	}
	body, ok := decodeOptionalBody[rerunBody](w, r)
	if !ok {
		return
	}

	res, err := h.store.RerunJob(r.Context(), store.RerunJobInput{
		JobRunID:    jobRunID,
		TriggeredBy: body.TriggeredBy,
	})
	switch {
	case err == nil:
		audit.Emit(r.Context(), h.log, h.store,
			store.AuditActionJobRerun, "job_run", res.JobRunID.String(),
			map[string]any{"run_id": res.RunID.String(), "attempt": res.Attempt})
		// A single-job rerun puts the parent run back to running, so the
		// PR check shouldn't stay red — re-open it as in_progress.
		if h.checks != nil {
			h.checks.ReportRunReopened(r.Context(), res.RunID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_run_id": res.JobRunID.String(),
			"run_id":     res.RunID.String(),
			"attempt":    res.Attempt,
		})
	case errors.Is(err, store.ErrJobRunNotFound):
		http.Error(w, "job_run not found", http.StatusNotFound)
	case errors.Is(err, store.ErrJobRunActive):
		http.Error(w, "job is still active — cancel first", http.StatusConflict)
	case errors.Is(err, store.ErrCannotRerunGate):
		http.Error(w, "an approval gate cannot be rerun — approve or reject it instead", http.StatusConflict)
	default:
		h.log.Error("rerun job", "job_run_id", jobRunID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

type triggerBody struct {
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// TriggerPipeline handles POST /api/v1/pipelines/{id}/trigger.
// Picks the pipeline's latest modification and queues a run. 422 when
// the pipeline has never seen a push (no modifications to replay).
func (h *Handler) TriggerPipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := chi.URLParam(r, "id")
	pipelineID, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "invalid pipeline id", http.StatusBadRequest)
		return
	}
	body, ok := decodeOptionalBody[triggerBody](w, r)
	if !ok {
		return
	}

	in := store.TriggerManualRunInput{
		PipelineID:  pipelineID,
		TriggeredBy: body.TriggeredBy,
	}
	// Always refresh HEAD before the trigger when we have a fetcher
	// wired. Users expect "Run latest" to run whatever is at HEAD of
	// the default branch right now, not the last modification cached
	// in the DB (which may be stale if no webhook is registered, or
	// if the operator is iterating locally and hasn't pushed enough
	// to fire one). InsertModification is idempotent on
	// (material_id, revision, branch) — if HEAD hasn't moved the
	// existing row is reused and nothing changes.
	if h.fetcher != nil {
		h.seedHeadModification(r.Context(), pipelineID)
	}
	res, err := h.store.TriggerManualRun(r.Context(), in)
	switch {
	case err == nil:
		audit.Emit(r.Context(), h.log, h.store,
			store.AuditActionRunTrigger, "pipeline", pipelineID.String(),
			map[string]any{"run_id": res.RunID.String(), "counter": res.Counter})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_id":      res.RunID.String(),
			"counter":     res.Counter,
			"pipeline_id": pipelineID.String(),
		})
	case errors.Is(err, store.ErrNoModificationForPipeline):
		http.Error(w, "pipeline has no modifications yet — push to a matched material first", http.StatusUnprocessableEntity)
	default:
		h.log.Error("trigger pipeline", "pipeline_id", pipelineID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// seedHeadModification fetches the HEAD commit SHA for a pipeline's
// first git material and inserts a modification row so TriggerManualRun
// can find one to execute against. Returns true when a usable
// modification is in the DB (freshly inserted or one that raced in
// between the first 422 and this attempt). Silent on failure — the
// caller keeps propagating the original 422 so the user sees the
// "push to seed" hint instead of a noisy 500.
//
// Resolves auth via the project's scm_source: the material URL must
// match a bound scm_source so we can pull its auth_ref (PAT/app token)
// for private repos. A material pointing at a repo without a bound
// scm_source falls back to unauthenticated — fine for public repos,
// will 404 for private ones, which is the expected behaviour.
func (h *Handler) seedHeadModification(ctx context.Context, pipelineID uuid.UUID) bool {
	mats, err := h.store.ListGitMaterialsForPipeline(ctx, pipelineID)
	if err != nil {
		h.log.Warn("trigger seed: list materials failed", "pipeline_id", pipelineID, "err", err)
		return false
	}
	if len(mats) == 0 {
		return false
	}
	// Stick to the first git material. Pipelines with multiple git
	// materials are rare and seeding all of them from HEAD could
	// mix unrelated revisions; the webhook path will correct the
	// picture on the first real push.
	m := mats[0]
	if m.Config.URL == "" {
		return false
	}
	branch := m.Config.Branch
	if branch == "" {
		branch = "main"
	}

	// Auth lookup — may legitimately miss (material points at an
	// unbound public repo). Pass an empty-URL SCMSource in that
	// case so the fetcher does an unauthenticated request.
	scm, err := h.store.FindSCMSourceByURL(ctx, m.Config.URL)
	if err != nil && !errors.Is(err, store.ErrSCMSourceNotFound) {
		h.log.Warn("trigger seed: scm_source lookup failed",
			"pipeline_id", pipelineID, "url", m.Config.URL, "err", err)
		return false
	}
	if errors.Is(err, store.ErrSCMSourceNotFound) {
		scm = store.SCMSource{Provider: "github", URL: m.Config.URL}
	}

	sha, err := h.fetcher.HeadSHA(ctx, scm, branch)
	if err != nil {
		if errors.Is(err, gh.ErrBranchNotFound) {
			h.log.Info("trigger seed: branch not found",
				"pipeline_id", pipelineID, "url", m.Config.URL, "branch", branch)
		} else {
			h.log.Warn("trigger seed: head lookup failed",
				"pipeline_id", pipelineID, "url", m.Config.URL, "branch", branch, "err", err)
		}
		return false
	}

	_, err = h.store.InsertModification(ctx, store.Modification{
		MaterialID: m.ID,
		Revision:   sha,
		Branch:     branch,
		Message:    fmt.Sprintf("seeded from HEAD of %s at manual trigger", branch),
	})
	if err != nil {
		h.log.Warn("trigger seed: insert modification failed",
			"pipeline_id", pipelineID, "material_id", m.ID, "err", err)
		return false
	}
	h.log.Info("trigger seed: modification inserted",
		"pipeline_id", pipelineID, "material_id", m.ID, "revision", sha, "branch", branch)
	return true
}

// --- helpers shared by the action endpoints ---

func parseRunID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

// decodeOptionalBody parses a JSON body when present. Empty body is
// fine (returns zero-value T); non-empty but malformed is 400.
func decodeOptionalBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var out T
	if r.Body == nil {
		return out, true
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return out, false
	}
	if len(raw) == 0 {
		return out, true
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return out, false
	}
	return out, true
}
