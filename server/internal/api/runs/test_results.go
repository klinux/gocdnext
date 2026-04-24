package runs

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

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

type testCaseHistoryDTO struct {
	ID             string `json:"id"`
	RunID          string `json:"run_id"`
	RunCounter     int64  `json:"run_counter"`
	PipelineName   string `json:"pipeline_name"`
	ProjectSlug    string `json:"project_slug"`
	Status         string `json:"status"`
	DurationMs     int64  `json:"duration_ms"`
	FailureMessage string `json:"failure_message,omitempty"`
	At             string `json:"at"`
}

type testCaseHistoryResponse struct {
	Entries []testCaseHistoryDTO `json:"entries"`
}

// defaultHistoryLimit covers "has this flaked in the last two
// weeks?" comfortably without forcing a big scan; callers that
// want more pass ?limit= up to historyHardCap.
const (
	defaultHistoryLimit = 14
	historyHardCap      = 100
)

// TestCaseHistory handles GET /api/v1/tests/history?classname=X&name=Y[&limit=N].
// Returns the most recent N executions of (classname, name)
// across every run of every pipeline so the Tests tab can show
// a green/red dot strip and identify flaky tests. No run id in
// the path — flakiness is a per-case property independent of
// any particular run.
func (h *Handler) TestCaseHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	classname := r.URL.Query().Get("classname")
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "`name` query param is required", http.StatusBadRequest)
		return
	}
	// classname CAN be empty — some frameworks (Go's testing) omit
	// it; in that case we look up by name alone, which may pull
	// same-named cases from unrelated packages. Users who want
	// precision will typically have a non-empty classname.

	limit := int32(defaultHistoryLimit)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid `limit`", http.StatusBadRequest)
			return
		}
		if parsed > historyHardCap {
			parsed = historyHardCap
		}
		limit = int32(parsed)
	}

	entries, err := h.store.TestCaseHistory(r.Context(), classname, name, limit)
	if err != nil {
		h.log.Error("test case history", "classname", classname, "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := testCaseHistoryResponse{
		Entries: make([]testCaseHistoryDTO, 0, len(entries)),
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, testCaseHistoryDTO{
			ID:             e.ID.String(),
			RunID:          e.RunID.String(),
			RunCounter:     e.RunCounter,
			PipelineName:   e.PipelineName,
			ProjectSlug:    e.ProjectSlug,
			Status:         e.Status,
			DurationMs:     e.DurationMs,
			FailureMessage: e.FailureMessage,
			At:             e.CreatedAt.Format(time.RFC3339Nano),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
