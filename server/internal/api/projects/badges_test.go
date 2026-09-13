package projects_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/api/projects"
	badgetoken "github.com/gocdnext/gocdnext/server/internal/badges"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newBadgeConfigRouter(t *testing.T) (http.Handler, *store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := projects.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithPublicBase("https://ci.example.com")
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{slug}/badge", h.GetBadgeConfig)
	r.Post("/api/v1/projects/{slug}/badge/token", h.RotateBadgeToken)
	r.Delete("/api/v1/projects/{slug}/badge/token", h.DisableBadgeToken)
	return r, s, pool
}

func TestProjectBadgeTokenRotateReturnedOnceAndStoredHashed(t *testing.T) {
	r, s, pool := newBadgeConfigRouter(t)
	ctx := context.Background()
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "demo"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	rr := doReq(r, http.MethodPost, "/api/v1/projects/demo/badge/token", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Enabled  bool   `json:"enabled"`
		Token    string `json:"token"`
		BadgeURL string `json:"badge_url"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Enabled || !badgetoken.LooksLikeToken(resp.Token) {
		t.Fatalf("response = %+v, want enabled + fresh token", resp)
	}
	if !strings.Contains(resp.BadgeURL, "https://ci.example.com/api/v1/badge/demo.svg?") ||
		!strings.Contains(resp.BadgeURL, "token="+resp.Token) ||
		!strings.Contains(resp.Markdown, resp.BadgeURL) {
		t.Fatalf("badge url/markdown = %q / %q", resp.BadgeURL, resp.Markdown)
	}

	var stored sql.NullString
	if err := pool.QueryRow(ctx, `SELECT badge_token_hash FROM projects WHERE slug='demo'`).Scan(&stored); err != nil {
		t.Fatalf("lookup token hash: %v", err)
	}
	if !stored.Valid || stored.String != badgetoken.HashToken(resp.Token) {
		t.Fatalf("stored hash = %v, want hash(token)", stored)
	}
	if stored.Valid && stored.String == resp.Token {
		t.Fatalf("stored plaintext token")
	}

	if rr := doReq(r, http.MethodGet, "/api/v1/projects/demo/badge", ""); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"enabled":true`) ||
		strings.Contains(rr.Body.String(), resp.Token) {
		t.Fatalf("GET = %d %s, want enabled without plaintext token", rr.Code, rr.Body.String())
	}

	if rr := doReq(r, http.MethodDelete, "/api/v1/projects/demo/badge/token", ""); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("DELETE = %d %s, want disabled", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodGet, "/api/v1/projects/demo/badge", ""); !strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("GET after delete = %s, want disabled", rr.Body.String())
	}

	page, err := s.ListAuditEvents(ctx, store.ListAuditEventsFilter{Action: store.AuditActionProjectBadgeRotate})
	if err != nil {
		t.Fatalf("list rotate audit: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("rotate audit total = %d, want 1", page.Total)
	}
	if strings.Contains(string(page.Events[0].Metadata), resp.Token) {
		t.Fatalf("audit metadata leaked token: %s", page.Events[0].Metadata)
	}
}

func TestProjectBadgeTokenUnknownProject(t *testing.T) {
	r, _, _ := newBadgeConfigRouter(t)
	if rr := doReq(r, http.MethodGet, "/api/v1/projects/nope/badge", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("GET unknown = %d, want 404", rr.Code)
	}
	if rr := doReq(r, http.MethodPost, "/api/v1/projects/nope/badge/token", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("POST unknown = %d, want 404", rr.Code)
	}
	if rr := doReq(r, http.MethodDelete, "/api/v1/projects/nope/badge/token", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown = %d, want 404", rr.Code)
	}
}
