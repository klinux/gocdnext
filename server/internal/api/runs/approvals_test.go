package runs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/api/runs"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedApprovalRun creates a project/pipeline with a single
// deploy stage holding one approval gate. Returns the run id and
// the gate's job_run_id so tests can hit the HTTP endpoint
// directly.
func seedApprovalRun(t *testing.T, pool *pgxpool.Pool, approvers []string) (runID, gateJobID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	slug := "appr-" + uuid.NewString()[:8]
	url, branch := "https://github.com/org/"+slug, "main"
	fp := domain.GitFingerprint(url, branch)
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "build", Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "gate", Stage: "deploy",
				Approval: &domain.ApprovalSpec{Approvers: approvers, Description: "Ship?"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var matID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID); err != nil {
		t.Fatal(err)
	}
	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     applied.Pipelines[0].PipelineID,
		MaterialID:     matID,
		ModificationID: 1,
		Revision:       "deadbeef",
		Branch:         branch,
		Provider:       "github",
		Delivery:       "t",
		TriggeredBy:    "test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID = res.RunID
	for _, jr := range res.JobRuns {
		if jr.Name == "gate" {
			gateJobID = jr.ID
		}
	}
	return
}

// approvalRouter wires just the approve/reject endpoints. Kept
// separate from doPost's hardcoded interface so the main actions
// suite doesn't have to grow to know about new endpoints.
func approvalRouter(h *runs.Handler) http.Handler {
	r := chi.NewRouter()
	r.Post("/api/v1/job_runs/{id}/approve", h.Approve)
	r.Post("/api/v1/job_runs/{id}/reject", h.Reject)
	return r
}

func doApprove(t *testing.T, h *runs.Handler, path, user string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req = req.WithContext(authapi.WithUser(req.Context(), store.User{Name: user}))
	}
	rr := httptest.NewRecorder()
	approvalRouter(h).ServeHTTP(rr, req)
	return rr
}

func TestApprove_HappyPath(t *testing.T) {
	h, pool := handler(t)
	_, gateJobID := seedApprovalRun(t, pool, []string{"alice"})

	rr := doApprove(t, h, "/api/v1/job_runs/"+gateJobID.String()+"/approve", "alice")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		JobRunID     string `json:"job_run_id"`
		RunID        string `json:"run_id"`
		RunCompleted bool   `json:"run_completed"`
		RunStatus    string `json:"run_status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if body.JobRunID != gateJobID.String() {
		t.Errorf("job_run_id = %q", body.JobRunID)
	}
	// Single-stage pipeline with one gate → approving closes the
	// whole run. Pins the cascade wiring end-to-end through HTTP.
	if !body.RunCompleted || body.RunStatus != "success" {
		t.Errorf("run cascade = completed=%v status=%q; want completed+success", body.RunCompleted, body.RunStatus)
	}

	// decided_by stamped with the authenticated user.
	var decidedBy string
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE(decided_by, '') FROM job_runs WHERE id = $1`, gateJobID).Scan(&decidedBy)
	if decidedBy != "alice" {
		t.Errorf("decided_by = %q, want alice (from UserFromContext)", decidedBy)
	}
}

func TestReject_HappyPath(t *testing.T) {
	h, pool := handler(t)
	_, gateJobID := seedApprovalRun(t, pool, []string{"alice"})

	rr := doApprove(t, h, "/api/v1/job_runs/"+gateJobID.String()+"/reject", "alice")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		RunStatus string `json:"run_status"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.RunStatus != "failed" {
		t.Errorf("run cascade = %q, want failed (rejection is a stage failure)", body.RunStatus)
	}
}

func TestApprove_NotInApproversReturns403(t *testing.T) {
	// The HTTP layer must relay the store's allow-list signal as
	// 403, not silently let the request through to the UPDATE.
	h, pool := handler(t)
	_, gateJobID := seedApprovalRun(t, pool, []string{"alice"})

	rr := doApprove(t, h, "/api/v1/job_runs/"+gateJobID.String()+"/approve", "mallory")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestApprove_SecondCallReturns409(t *testing.T) {
	// Concurrent Approve or double-click — the second request
	// must map ErrApprovalNotPending to 409 so the UI can show
	// "already decided" instead of a generic error.
	h, pool := handler(t)
	_, gateJobID := seedApprovalRun(t, pool, []string{})

	if rr := doApprove(t, h, "/api/v1/job_runs/"+gateJobID.String()+"/approve", "alice"); rr.Code != http.StatusAccepted {
		t.Fatalf("first approve: %d", rr.Code)
	}
	rr := doApprove(t, h, "/api/v1/job_runs/"+gateJobID.String()+"/approve", "bob")
	if rr.Code != http.StatusConflict {
		t.Errorf("second approve status = %d, want 409", rr.Code)
	}
}

func TestApprove_UnknownIDReturns404(t *testing.T) {
	h, _ := handler(t)
	rr := doApprove(t, h, "/api/v1/job_runs/11111111-1111-1111-1111-111111111111/approve", "alice")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestApprove_InvalidIDReturns400(t *testing.T) {
	h, _ := handler(t)
	rr := doApprove(t, h, "/api/v1/job_runs/not-a-uuid/approve", "alice")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestApprove_AgainstRegularJobReturns404(t *testing.T) {
	// Approving a non-gate job_run must be treated as "not found"
	// rather than silently flipping a regular queued job to
	// success — the HTTP layer's 404 carries through the store's
	// ErrApprovalGateNotFound for both "unknown id" and "id isn't
	// a gate" to avoid leaking row existence.
	h, pool := handler(t)
	runID, _ := seedRunWithModification(t, pool)
	var regularJob uuid.UUID
	_ = pool.QueryRow(context.Background(),
		`SELECT id FROM job_runs WHERE run_id = $1 LIMIT 1`, runID).Scan(&regularJob)

	rr := doApprove(t, h, "/api/v1/job_runs/"+regularJob.String()+"/approve", "alice")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d", rr.Code)
	}
}
