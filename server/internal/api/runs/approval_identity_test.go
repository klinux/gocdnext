package runs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/api/runs"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func doApproveAs(t *testing.T, h *runs.Handler, path string, u store.User) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req = req.WithContext(authapi.WithUser(req.Context(), u))
	rr := httptest.NewRecorder()
	approvalRouter(h).ServeHTTP(rr, req)
	return rr
}

// #172: a service-account token must not cast a vote, even on an open gate.
// Before, its synthetic id hit the users(id) FK and surfaced as a 500.
func TestApprove_ServiceAccountReturns403(t *testing.T) {
	h, pool := handler(t)
	_, gate := seedApprovalRun(t, pool, []string{})

	for _, verb := range []string{"approve", "reject"} {
		rr := doApproveAs(t, h, "/api/v1/job_runs/"+gate.String()+"/"+verb, store.User{
			ID: uuid.New(), Name: "deploy-bot", Email: "deploy-bot@service-account",
			Provider: store.ProviderServiceAccount, Role: store.RoleAdmin,
		})
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "service account") {
			t.Fatalf("%s status = %d body=%q, want 403 naming the service account", verb, rr.Code, rr.Body.String())
		}
	}
}

// The handler forwards the stable subject, so a `sub:` entry matches the
// user however their display name or email has changed since.
func TestApprove_SubjectRefMatchesThroughHTTP(t *testing.T) {
	h, pool := handler(t)
	_, gate := seedApprovalRun(t, pool, []string{"sub:oidc:sub-alice"})
	alice, err := store.New(pool).UpsertUserByProvider(context.Background(), store.UpsertUserInput{
		Email: "alice@corp.com", Name: "Alice", Provider: "oidc", ExternalID: "sub-alice",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	alice.Name = "Alice Renamed"

	if rr := doApproveAs(t, h, "/api/v1/job_runs/"+gate.String()+"/approve", alice); rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

type downGroupSource struct{}

func (downGroupSource) ApproverGroupNames(context.Context, store.ApproverIdentity) ([]string, error) {
	return nil, errors.New("dial tcp 10.0.0.1:443: connection refused")
}

// External group resolution failing is a retryable 503, and the upstream error
// text stays in the server log, not the response.
func TestApprove_GroupSourceDownReturns503(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool).WithApproverGroupSource(downGroupSource{})
	h := runs.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, gate := seedApprovalRun(t, pool, nil)
	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET approver_groups = '{platform-sre}' WHERE id = $1`, gate); err != nil {
		t.Fatal(err)
	}
	alice, err := s.UpsertUserByProvider(context.Background(), store.UpsertUserInput{
		Email: "alice@corp.com", Name: "Alice", Provider: "oidc", ExternalID: "sub-alice",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rr := doApproveAs(t, h, "/api/v1/job_runs/"+gate.String()+"/approve", alice)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "10.0.0.1") {
		t.Fatalf("response leaks the upstream error: %q", rr.Body.String())
	}
}
