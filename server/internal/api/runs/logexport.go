package runs

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// LogExport handles GET /api/v1/runs/{id}/jobs/{jobId}/log.txt —
// the full job log as a text/plain attachment (#37 download). The
// store streams rows straight into the response writer; no size
// cap because the response is chunked and the client decides how
// much to keep.
//
// Known boundary: logs past the retention/archive window live in
// the artifact store, not log_lines — those export empty here. The
// archive viewer remains the path for archived logs; unifying the
// two sources is follow-up work, noted in #37.
func (h *Handler) LogExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	// Scope check: the job must belong to the run in the URL —
	// otherwise any authenticated viewer could walk job UUIDs
	// across runs they weren't looking at.
	ok, err := h.store.JobBelongsToRun(r.Context(), jobID, runID)
	if err != nil {
		h.log.Error("log export: scope check", "run_id", runID, "job_id", jobID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "job-"+jobID.String()[:8]+".log"))
	n, err := h.store.WriteJobLogText(r.Context(), jobID, w)
	if err != nil {
		// Headers may already be out — log; the truncated body is
		// the best signal the client gets mid-stream.
		h.log.Error("log export: stream", "run_id", runID, "job_id", jobID, "lines", n, "err", err)
		return
	}
	h.log.Info("log export served", "run_id", runID, "job_id", jobID, "lines", n)
}
