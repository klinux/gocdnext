package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/scm"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// dispatchHTTP drives the real dispatchPullRequest at the HTTP boundary and
// returns the recorder + the delivery record, so a test can assert the status
// code AND the persisted (sanitized) audit fields.
func dispatchHTTP(t *testing.T, h *Handler, ev pullRequestEvent, body []byte) (*httptest.ResponseRecorder, *deliveryRec) {
	t.Helper()
	rr := httptest.NewRecorder()
	sc := &statusCapture{ResponseWriter: rr}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", nil)
	rec := &deliveryRec{provider: "github", event: "pull_request", writer: sc}
	h.dispatchPullRequest(sc, req, body, "delivery-http", rec, ev)
	return rr, rec
}

type prHeadHTTPBody struct {
	Runs  []map[string]any `json:"runs"`
	Error string           `json:"error"`
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) prHeadHTTPBody {
	t.Helper()
	var b prHeadHTTPBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	return b
}

// A valid mixed set at the HTTP boundary → 202 with BOTH runs in the body.
func TestDispatchPR_ValidReturns202(t *testing.T) {
	h, _, f, _, _, _ := seedWiring(t)
	f.files = []scm.RawFile{rawFile("build.yaml", "build")}
	rr, rec := dispatchHTTP(t, h, wireEvent(), []byte("{}"))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if got := len(decodeBody(t, rr).Runs); got != 2 {
		t.Fatalf("runs = %d, want 2 (gov base + build head)", got)
	}
	if rec.status != store.WebhookStatusAccepted {
		t.Fatalf("rec.status = %q, want accepted", rec.status)
	}
}

// Finding #1: an ambiguous binding is SURFACED (422), not a silent 202 with
// runs:[]. The mandatory system_managed pipeline still runs and rides in the body.
func TestDispatchPR_AmbiguousBindingReturns422(t *testing.T) {
	h, s, f, pool, ctx, _ := seedWiring(t)
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo2", Name: "demo2",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: wireURL, DefaultBranch: wireBranch},
	}); err != nil {
		t.Fatalf("apply demo2: %v", err)
	}
	if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "demo2", true); err != nil {
		t.Fatalf("enable demo2 toggle: %v", err)
	}

	rr, rec := dispatchHTTP(t, h, wireEvent(), []byte("{}"))

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body.Error != "pr-head: ambiguous scm binding" {
		t.Fatalf("body.error = %q, want stable ambiguous message", body.Error)
	}
	if rec.errText != "pr-head: ambiguous scm binding" {
		t.Fatalf("rec.errText = %q, want the stable ambiguous message", rec.errText)
	}
	if len(body.Runs) != 1 { // the gov base run still created + reported
		t.Fatalf("runs = %d, want 1 (gov base run in the body)", len(body.Runs))
	}
	if n := runCount(t, pool, ctx, "gov"); n != 1 {
		t.Fatalf("gov runs = %d, want 1 (system_managed unaffected)", n)
	}
	if n := runCount(t, pool, ctx, "build"); n != 0 {
		t.Fatalf("build runs = %d, want 0 (repo blocked)", n)
	}
	if f.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (blocked before fetch)", f.calls)
	}
}

// Finding #1 + #2: a fetch failure on a MIXED project returns 503 EVEN THOUGH the
// system_managed run was created (the provider must retry the repo pipeline), and
// the persisted error is the STABLE category message — never the raw "boom".
func TestDispatchPR_FetchErrorReturns503WithBaseRun(t *testing.T) {
	h, _, f, pool, ctx, _ := seedWiring(t)
	f.err = errors.New("boom-secret-detail")

	rr, rec := dispatchHTTP(t, h, wireEvent(), []byte("{}"))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if len(body.Runs) != 1 {
		t.Fatalf("runs = %d, want 1 (gov base run rides in the 503 body)", len(body.Runs))
	}
	if n := runCount(t, pool, ctx, "gov"); n != 1 {
		t.Fatalf("gov runs = %d, want 1 (system_managed created despite 503)", n)
	}
	if n := runCount(t, pool, ctx, "build"); n != 0 {
		t.Fatalf("build runs = %d, want 0 (repo blocked, provider retries)", n)
	}
	// Finding #2: the raw fetch detail must NOT leak into the persisted error or
	// the wire body — only the stable per-category string.
	if rec.errText != "pr-head: config temporarily unavailable" {
		t.Fatalf("rec.errText = %q, want the sanitized category message", rec.errText)
	}
	if body.Error == "boom-secret-detail" || rr.Body.Len() == 0 {
		t.Fatalf("raw fetch detail leaked to the body: %s", rr.Body.String())
	}
}

// Finding (round 3): a MISSING binding (0 rows) is surfaced as 422, distinct from
// "all toggles off" (a legitimate base flow). Covers both a head-only project —
// the original silent-202 regression — and a mixed one.
func TestDispatchPR_MissingBindingReturns422(t *testing.T) {
	t.Run("mixed → 422 with the system_managed run in the body", func(t *testing.T) {
		h, _, _, pool, ctx, _ := seedWiring(t)
		if _, err := pool.Exec(ctx, `DELETE FROM scm_sources`); err != nil {
			t.Fatalf("delete scm_sources: %v", err)
		}
		rr, rec := dispatchHTTP(t, h, wireEvent(), []byte("{}"))
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
		}
		body := decodeBody(t, rr)
		if body.Error != "pr-head: no scm binding for repo" || rec.errText != "pr-head: no scm binding for repo" {
			t.Fatalf("error = %q / errText = %q, want stable missing-binding message", body.Error, rec.errText)
		}
		if len(body.Runs) != 1 {
			t.Fatalf("runs = %d, want 1 (gov base run rides in the 422 body)", len(body.Runs))
		}
		if n := runCount(t, pool, ctx, "gov"); n != 1 {
			t.Fatalf("gov runs = %d, want 1", n)
		}
		if n := runCount(t, pool, ctx, "build"); n != 0 {
			t.Fatalf("build runs = %d, want 0 (repo blocked)", n)
		}
	})
	t.Run("head-only → 422 with runs:[] (no silent 202)", func(t *testing.T) {
		h, _, _, pool, ctx, _ := seedWiring(t)
		// Both pipelines are repo (no system_managed) → a head-only project.
		if _, err := pool.Exec(ctx, `UPDATE pipelines SET system_managed = false`); err != nil {
			t.Fatalf("clear system_managed: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM scm_sources`); err != nil {
			t.Fatalf("delete scm_sources: %v", err)
		}
		rr, _ := dispatchHTTP(t, h, wireEvent(), []byte("{}"))
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (not a silent 202); body=%s", rr.Code, rr.Body.String())
		}
		if got := len(decodeBody(t, rr).Runs); got != 0 {
			t.Fatalf("runs = %d, want 0 (every repo pipeline blocked)", got)
		}
		if n := runCount(t, pool, ctx, "build") + runCount(t, pool, ctx, "gov"); n != 0 {
			t.Fatalf("runs created = %d, want 0 (all repo, binding missing)", n)
		}
	})
}

// A parseable-but-invalid head (authorized pipeline absent) → 422.
func TestDispatchPR_InvalidConfigReturns422(t *testing.T) {
	h, _, f, _, _, _ := seedWiring(t)
	f.files = []scm.RawFile{} // build authorized but absent from head
	rr, _ := dispatchHTTP(t, h, wireEvent(), []byte("{}"))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

// blockingFetcher blocks until the resolution context is canceled, then returns
// its error — so the per-Handler resolution timeout is what unblocks it.
type blockingFetcher struct{}

func (blockingFetcher) Fetch(ctx context.Context, _ store.SCMSource, _, _ string) ([]scm.RawFile, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingFetcher) HeadSHA(_ context.Context, _ store.SCMSource, _ string) (string, error) {
	return "", nil
}

// Finding #5: the resolution timeout bounds a slow head — a fetcher that never
// returns is cut off by prHeadResolveTimeout and mapped to 503 (retryable), while
// the mandatory system_managed run was already dispatched base-first.
func TestDispatchPR_ResolutionTimeoutReturns503(t *testing.T) {
	h, _, _, pool, ctx, _ := seedWiring(t)
	h.fetcher = blockingFetcher{}
	h.prHeadResolveTimeout = 80 * time.Millisecond

	start := time.Now()
	rr, _ := dispatchHTTP(t, h, wireEvent(), []byte("{}"))
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("resolution not bounded: took %s", elapsed)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (timeout is retryable); body=%s", rr.Code, rr.Body.String())
	}
	if n := runCount(t, pool, ctx, "gov"); n != 1 {
		t.Fatalf("gov runs = %d, want 1 (base ran base-first, not blocked by the slow head)", n)
	}
}
