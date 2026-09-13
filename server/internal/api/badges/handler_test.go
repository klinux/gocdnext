package badges

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	badgetoken "github.com/gocdnext/gocdnext/server/internal/badges"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

type fakeBadgeStore struct {
	calls    int
	slug     string
	hash     string
	pipeline string
	branch   string
	run      store.BadgeRun
	ok       bool
	err      error
}

func (f *fakeBadgeStore) GetBadgeLatestRun(ctx context.Context, slug, tokenHash, pipelineName, branch string) (store.BadgeRun, bool, error) {
	f.calls++
	f.slug = slug
	f.hash = tokenHash
	f.pipeline = pipelineName
	f.branch = branch
	return f.run, f.ok, f.err
}

func TestBadge_MissingOrInvalidTokenReturnsUnknownWithoutLookup(t *testing.T) {
	fake := &fakeBadgeStore{}
	h := NewHandler(fake, slog.Default(), "https://ci.example.com")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/badge/demo/build.svg", nil)
	rr := httptest.NewRecorder()

	h.Pipeline(rr, req.WithContext(req.Context()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if fake.calls != 0 {
		t.Fatalf("store calls = %d, want 0", fake.calls)
	}
	if body := rr.Body.String(); !strings.Contains(body, "unknown") || strings.Contains(body, "/runs/") {
		t.Fatalf("body = %s, want unknown badge without run link", body)
	}
	if got := rr.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestBadge_ValidTokenRendersLatestRunAndLink(t *testing.T) {
	token := strings.Repeat("a", badgetoken.TokenEncodedLength)
	runID := uuid.New()
	fake := &fakeBadgeStore{
		run: store.BadgeRun{
			ID:           runID,
			Status:       string(domain.StatusSuccess),
			PipelineName: "build",
			CreatedAt:    time.Now(),
		},
		ok: true,
	}
	h := NewHandler(fake, slog.Default(), "https://ci.example.com/")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/badge/demo/build.svg?token="+token+"&branch=main", nil)
	rr := httptest.NewRecorder()

	h.serve(rr, req, "demo", "build")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if fake.calls != 1 || fake.slug != "demo" || fake.pipeline != "build" || fake.branch != "main" {
		t.Fatalf("lookup = calls:%d slug:%q pipeline:%q branch:%q", fake.calls, fake.slug, fake.pipeline, fake.branch)
	}
	if fake.hash != badgetoken.HashToken(token) {
		t.Fatalf("hash = %q, want token hash", fake.hash)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "passing") || !strings.Contains(body, "https://ci.example.com/runs/"+runID.String()) {
		t.Fatalf("body = %s, want passing badge linked to run", body)
	}
	if got := rr.Header().Get("Content-Type"); got != "image/svg+xml; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rr.Header().Get("Content-Security-Policy"); got != "default-src 'none'; script-src 'none'; object-src 'none'; base-uri 'none'" {
		t.Fatalf("Content-Security-Policy = %q", got)
	}

	etag := rr.Header().Get("ETag")
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/badge/demo/build.svg?token="+token, nil)
	req2.Header.Set("If-None-Match", etag)
	rr2 := httptest.NewRecorder()
	h.serve(rr2, req2, "demo", "build")
	if rr2.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", rr2.Code)
	}
}

func TestBadge_MountExtractsProjectAndPipelineFromSVGPath(t *testing.T) {
	token := strings.Repeat("c", badgetoken.TokenEncodedLength)
	fake := &fakeBadgeStore{run: store.BadgeRun{ID: uuid.New(), Status: string(domain.StatusRunning)}, ok: true}
	h := NewHandler(fake, slog.Default(), "")
	r := chi.NewRouter()
	h.Mount(r)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/badge/demo/build.svg?token="+token, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if fake.slug != "demo" || fake.pipeline != "build" {
		t.Fatalf("route params slug=%q pipeline=%q, want demo/build", fake.slug, fake.pipeline)
	}
	if !strings.Contains(rr.Body.String(), "running") {
		t.Fatalf("body = %s, want running badge", rr.Body.String())
	}
}

func TestBadge_LookupErrorIsUnknownAndNotCached(t *testing.T) {
	token := strings.Repeat("b", badgetoken.TokenEncodedLength)
	fake := &fakeBadgeStore{err: errors.New("db down")}
	h := NewHandler(fake, slog.Default(), "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/badge/demo.svg?token="+token, nil)
	rr := httptest.NewRecorder()

	h.serve(rr, req, "demo", "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unknown") {
		t.Fatalf("body = %s, want unknown", rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
