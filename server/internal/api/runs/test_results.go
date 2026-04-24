package runs

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// testCaseDTO is the read-side shape for one parsed case. Flat
// JSON mirrors the store.TestResultCase fields with snake_case
// keys so the client types match the rest of /api/v1.
type testCaseDTO struct {
	ID             string `json:"id"`
	JobRunID       string `json:"job_run_id"`
	Suite          string `json:"suite"`
	Classname      string `json:"classname,omitempty"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	DurationMs     int64  `json:"duration_ms"`
	FailureType    string `json:"failure_type,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
	FailureDetail  string `json:"failure_detail,omitempty"`
}

type testSummaryDTO struct {
	JobRunID   string `json:"job_run_id"`
	Total      int64  `json:"total"`
	Passed     int64  `json:"passed"`
	Failed     int64  `json:"failed"`
	Skipped    int64  `json:"skipped"`
	Errored    int64  `json:"errored"`
	DurationMs int64  `json:"duration_ms"`
}

type testResultsResponse struct {
	Summaries []testSummaryDTO `json:"summaries"`
	Cases     []testCaseDTO    `json:"cases"`
}

// TestResults handles GET /api/v1/runs/{id}/tests. Returns the
// per-job aggregate summaries AND the full case list for every
// job in the run; the UI splits them by job_run_id client-side
// so rendering the tab is one fetch instead of N.
func (h *Handler) TestResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	// Pull the full run detail to enumerate every job_run_id. Log
	// fetch is cheap (logs=0 skips the tail query); what matters
	// is the stage/job tree so the client can attribute cases
	// back to the correct job card.
	detail, err := h.store.GetRunDetail(r.Context(), runID, 0, nil)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.log.Error("test results: load run", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	jobIDs := make([]uuid.UUID, 0, 16)
	for _, st := range detail.Stages {
		for _, j := range st.Jobs {
			jobIDs = append(jobIDs, j.ID)
		}
	}

	cases, summaries, err := h.store.TestResultsByRun(r.Context(), jobIDs)
	if err != nil {
		h.log.Error("test results", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := testResultsResponse{
		Summaries: make([]testSummaryDTO, 0, len(summaries)),
		Cases:     make([]testCaseDTO, 0, len(cases)),
	}
	for _, s := range summaries {
		out.Summaries = append(out.Summaries, testSummaryDTO{
			JobRunID:   s.JobRunID.String(),
			Total:      s.Total,
			Passed:     s.Passed,
			Failed:     s.Failed,
			Skipped:    s.Skipped,
			Errored:    s.Errored,
			DurationMs: s.DurationMs,
		})
	}
	for _, c := range cases {
		out.Cases = append(out.Cases, testCaseDTO{
			ID:             c.ID.String(),
			JobRunID:       c.JobRunID.String(),
			Suite:          c.Suite,
			Classname:      c.Classname,
			Name:           c.Name,
			Status:         c.Status,
			DurationMs:     c.DurationMs,
			FailureType:    c.FailureType,
			FailureMessage: c.FailureMessage,
			FailureDetail:  c.FailureDetail,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
