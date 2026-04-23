package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	gh "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Cancel handles POST /api/v1/runs/{id}/cancel.
// Response:
//   202 Accepted   — the run was active and is now canceled
//   404 Not Found  — unknown run id
//   409 Conflict   — run already in a terminal status
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

// rerunBody optional JSON body for Rerun. triggered_by lands on the
// new run row so the UI can show who asked. Omit it and the store
// synthesizes "rerun:<orig>" automatically.
type rerunBody struct {
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// Rerun handles POST /api/v1/runs/{id}/rerun.
// Response on success (202):
//   { "run_id": "...", "counter": <int>, "rerun_of": "<orig>" }
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
//   202 Accepted → { job_run_id, run_id, attempt }
//   404          → unknown job_run id
//   409          → job still active (queued/running); cancel first
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
