package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/scm"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

const wireURL = "https://github.com/acme/demo"
const wireBranch = "main"

func wireCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

func gitPipeline(name, fp string) *domain.Pipeline {
	return &domain.Pipeline{
		Name: name, Stages: []string{"build"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: wireURL, Branch: wireBranch, Events: []string{"pull_request"}},
		}},
		Jobs: []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
	}
}

// seedWiring applies a project bound to wireURL with a repo pipeline "build" and
// a "gov" pipeline (marked system_managed), each with a material of the PR's
// fingerprint, and returns a Handler wired to a fake fetcher + the matched
// materials. The toggle is ON.
func seedWiring(t *testing.T) (*Handler, *store.Store, *fakeCfgFetcher, *pgxpool.Pool, context.Context, []store.Material) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(wireCipher(t))
	ctx := context.Background()
	fp := store.FingerprintFor(wireURL, wireBranch)

	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "demo",
		Pipelines: []*domain.Pipeline{gitPipeline("build", fp), gitPipeline("gov", fp)},
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: wireURL, DefaultBranch: wireBranch},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE pipelines SET system_managed = true WHERE name = 'gov'`); err != nil {
		t.Fatalf("mark gov system_managed: %v", err)
	}
	if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "demo", true); err != nil {
		t.Fatalf("enable toggle: %v", err)
	}
	materials, err := s.FindMaterialsByFingerprint(ctx, fp)
	if err != nil || len(materials) != 2 {
		t.Fatalf("materials = %d (err=%v), want 2 (build + gov)", len(materials), err)
	}
	f := &fakeCfgFetcher{}
	h := &Handler{store: s, log: slog.New(slog.NewTextHandler(io.Discard, nil)), fetcher: f}
	return h, s, f, pool, ctx, materials
}

func wireEvent() pullRequestEvent {
	return pullRequestEvent{
		Provider: "github", SameRepo: true, Number: 7, Title: "a pr", Author: "dev",
		HeadSHA: "headsha0", HeadRef: "feature/x", BaseRef: wireBranch,
		CloneURL: wireURL, RepoLabel: "acme/demo",
	}
}

func wireCauseDetail() json.RawMessage {
	b, _ := json.Marshal(map[string]any{"pr_number": 7, "pr_title": "a pr"})
	return b
}

func runCount(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM runs r JOIN pipelines pl ON pl.id = r.pipeline_id WHERE pl.name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("run count: %v", err)
	}
	return n
}

func materialIDs(ms []store.Material) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, m := range ms {
		out[m.ID] = true
	}
	return out
}

// Proof 3: a valid mixed set — system_managed runs on the base flow, the repo
// pipeline runs from the head. build gets a pr_head run; gov is returned to base.
func TestApplyPRHeadConfig_MixedValid(t *testing.T) {
	h, s, f, pool, ctx, materials := seedWiring(t)
	f.files = []scm.RawFile{rawFile("build.yaml", "build")}

	base := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "delivery-1", []byte("{}"))

	// gov (system_managed) returned to base; build removed (ran via head).
	if len(base) != 1 {
		t.Fatalf("base = %d, want 1 (gov only)", len(base))
	}
	if f.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", f.calls)
	}
	if n := runCount(t, pool, ctx, "build"); n != 1 {
		t.Fatalf("build runs = %d, want 1 (pr_head)", n)
	}
	if n := runCount(t, pool, ctx, "gov"); n != 0 {
		t.Fatalf("gov runs = %d, want 0 (base flow runs later, not here)", n)
	}
	// The build run is a pr_head run.
	var src string
	if err := pool.QueryRow(ctx,
		`SELECT r.cause_detail->>'config_source' FROM runs r JOIN pipelines pl ON pl.id=r.pipeline_id WHERE pl.name='build'`).
		Scan(&src); err != nil {
		t.Fatalf("cause_detail: %v", err)
	}
	if src != "pr_head" {
		t.Fatalf("config_source = %q, want pr_head", src)
	}
	_ = s
}

// Proof 2: a mixed set where head resolution fails — system_managed still runs
// (returned to base), the repo pipeline is blocked (no run, no partial write).
func TestApplyPRHeadConfig_MixedResolveFailBlocksRepoOnly(t *testing.T) {
	h, _, f, pool, ctx, materials := seedWiring(t)
	f.files = []scm.RawFile{} // build absent from head → resolve fails (authorized but missing)

	base := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "delivery-1", []byte("{}"))

	if len(base) != 1 {
		t.Fatalf("base = %d, want 1 (gov still runs base)", len(base))
	}
	if n := runCount(t, pool, ctx, "build"); n != 0 {
		t.Fatalf("build runs = %d, want 0 (repo blocked on resolve failure)", n)
	}
}

// Proof 1: only system_managed work → base flow, and ZERO fetch (a head that
// deleted `.gocdnext/` can't block the mandatory pipeline).
func TestApplyPRHeadConfig_OnlySystemManagedNoFetch(t *testing.T) {
	h, _, f, pool, ctx, materials := seedWiring(t)
	// Mark BOTH pipelines system_managed → no repo pipelines to resolve.
	if _, err := pool.Exec(ctx, `UPDATE pipelines SET system_managed = true`); err != nil {
		t.Fatalf("mark all system_managed: %v", err)
	}
	base := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "delivery-1", []byte("{}"))
	if len(base) != len(materials) {
		t.Fatalf("base = %d, want %d (all base)", len(base), len(materials))
	}
	if f.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (only system_managed → no fetch)", f.calls)
	}
}

// Fork / non-github → base flow, zero fetch. Toggle off → repo runs base, zero
// fetch. Both keep the base flow byte-for-byte and never touch the head.
func TestApplyPRHeadConfig_ForkAndOffAreZeroFetch(t *testing.T) {
	t.Run("fork (SameRepo=false)", func(t *testing.T) {
		h, _, f, _, ctx, materials := seedWiring(t)
		ev := wireEvent()
		ev.SameRepo = false
		base := h.applyPRHeadConfig(ctx, ev, materials, wireCauseDetail(), "d", []byte("{}"))
		if len(base) != len(materials) || f.calls != 0 {
			t.Fatalf("base=%d fetch=%d, want %d/0", len(base), f.calls, len(materials))
		}
	})
	t.Run("toggle off", func(t *testing.T) {
		h, s, f, _, ctx, materials := seedWiring(t)
		if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "demo", false); err != nil {
			t.Fatalf("disable: %v", err)
		}
		base := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "d", []byte("{}"))
		if len(base) != len(materials) || f.calls != 0 {
			t.Fatalf("base=%d fetch=%d, want %d/0 (off → base, no fetch)", len(base), f.calls, len(materials))
		}
	})
}

// Ambiguous binding (the clone URL bound to >1 project) → repo pipelines blocked
// fail-closed; system_managed still returns to base; no fetch.
func TestApplyPRHeadConfig_AmbiguousBindingBlocksRepo(t *testing.T) {
	h, s, f, pool, ctx, materials := seedWiring(t)
	// A second project bound to the SAME url → the URL now resolves to 2 sources.
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo2", Name: "demo2",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: wireURL, DefaultBranch: wireBranch},
	}); err != nil {
		t.Fatalf("apply demo2: %v", err)
	}

	base := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "d", []byte("{}"))

	if ids := materialIDs(base); len(ids) != 1 {
		t.Fatalf("base = %d, want 1 (gov only; build blocked)", len(base))
	}
	if n := runCount(t, pool, ctx, "build"); n != 0 {
		t.Fatalf("build runs = %d, want 0 (ambiguous binding blocks repo)", n)
	}
	if f.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (blocked before fetch)", f.calls)
	}
}
