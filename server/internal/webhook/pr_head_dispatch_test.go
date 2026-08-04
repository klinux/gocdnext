package webhook

import (
	"context"
	"encoding/json"
	"errors"
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

// configSource returns the config_source stamped on a pipeline's newest run
// ("pr_head" for a head run, empty/absent for a base run).
func configSource(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name string) string {
	t.Helper()
	var src *string
	if err := pool.QueryRow(ctx,
		`SELECT r.cause_detail->>'config_source' FROM runs r JOIN pipelines pl ON pl.id=r.pipeline_id
		 WHERE pl.name=$1 ORDER BY r.created_at DESC LIMIT 1`, name).Scan(&src); err != nil {
		t.Fatalf("config_source(%s): %v", name, err)
	}
	if src == nil {
		return ""
	}
	return *src
}

// Proof 3: a valid mixed set — system_managed runs on the base flow, the repo
// pipeline runs from the head. Base-first: BOTH are created here (gov base + build
// pr_head), and gov is dispatched before the fetch even happens.
func TestApplyPRHeadConfig_MixedValid(t *testing.T) {
	h, _, f, pool, ctx, materials := seedWiring(t)
	f.files = []scm.RawFile{rawFile("build.yaml", "build")}

	outcomes, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "delivery-1", []byte("{}"))
	if resolveErr != nil {
		t.Fatalf("resolveErr = %v, want nil", resolveErr)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2 (gov base + build head)", len(outcomes))
	}
	if f.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", f.calls)
	}
	if n := runCount(t, pool, ctx, "build"); n != 1 {
		t.Fatalf("build runs = %d, want 1 (pr_head)", n)
	}
	if n := runCount(t, pool, ctx, "gov"); n != 1 {
		t.Fatalf("gov runs = %d, want 1 (base flow, dispatched here base-first)", n)
	}
	if src := configSource(t, pool, ctx, "build"); src != "pr_head" {
		t.Fatalf("build config_source = %q, want pr_head", src)
	}
	if src := configSource(t, pool, ctx, "gov"); src == "pr_head" {
		t.Fatalf("gov config_source = %q, want base (not pr_head)", src)
	}
}

// Proof 2: a mixed set where head resolution fails — system_managed still runs on
// the base flow (base-first), the repo pipeline is blocked (no run, no partial
// write) and a TYPED error is returned.
func TestApplyPRHeadConfig_MixedResolveFailBlocksRepoOnly(t *testing.T) {
	h, _, f, pool, ctx, materials := seedWiring(t)
	f.files = []scm.RawFile{} // build absent from head → resolve fails (authorized but missing)

	outcomes, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "delivery-1", []byte("{}"))

	if !errors.Is(resolveErr, ErrPRHeadConfigInvalid) {
		t.Fatalf("resolveErr = %v, want ErrPRHeadConfigInvalid", resolveErr)
	}
	if n := runCount(t, pool, ctx, "gov"); n != 1 {
		t.Fatalf("gov runs = %d, want 1 (system_managed still runs base)", n)
	}
	if n := runCount(t, pool, ctx, "build"); n != 0 {
		t.Fatalf("build runs = %d, want 0 (repo blocked on resolve failure)", n)
	}
	// The gov base run is still in the outcomes so the caller can report it.
	if len(outcomes) != 1 || outcomes[0].RunID == uuid.Nil {
		t.Fatalf("outcomes = %+v, want the 1 created gov base run", outcomes)
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
	outcomes, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "delivery-1", []byte("{}"))
	if resolveErr != nil || len(outcomes) != len(materials) {
		t.Fatalf("outcomes=%d err=%v, want %d/nil (all base)", len(outcomes), resolveErr, len(materials))
	}
	if f.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (only system_managed → no fetch)", f.calls)
	}
}

// Fork / non-github → base flow, zero fetch. Toggle off → repo runs base, zero
// fetch. Both keep the base flow (all materials fanned out) and never touch the
// head.
func TestApplyPRHeadConfig_ForkAndOffAreZeroFetch(t *testing.T) {
	assertAllBaseNoFetch := func(t *testing.T, outcomes []fanOutOutcome, resolveErr error, f *fakeCfgFetcher, materials []store.Material) {
		t.Helper()
		if resolveErr != nil || len(outcomes) != len(materials) || f.calls != 0 {
			t.Fatalf("outcomes=%d err=%v fetch=%d, want %d/nil/0 (base, no fetch)",
				len(outcomes), resolveErr, f.calls, len(materials))
		}
	}
	t.Run("fork (SameRepo=false)", func(t *testing.T) {
		h, _, f, _, ctx, materials := seedWiring(t)
		ev := wireEvent()
		ev.SameRepo = false
		outcomes, resolveErr := h.applyPRHeadConfig(ctx, ev, materials, wireCauseDetail(), "d", []byte("{}"))
		assertAllBaseNoFetch(t, outcomes, resolveErr, f, materials)
	})
	t.Run("toggle off", func(t *testing.T) {
		h, s, f, _, ctx, materials := seedWiring(t)
		if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "demo", false); err != nil {
			t.Fatalf("disable: %v", err)
		}
		outcomes, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "d", []byte("{}"))
		assertAllBaseNoFetch(t, outcomes, resolveErr, f, materials)
	})
	// GitLab / Bitbucket never reach the head path (SameRepo is a GitHub-only
	// signal). The provider gate blocks even a (hypothetical) same-repo MR/PR, so
	// they always run the base flow with ZERO fetch.
	for _, provider := range []string{"gitlab", "bitbucket"} {
		t.Run(provider+" provider", func(t *testing.T) {
			h, _, f, _, ctx, materials := seedWiring(t)
			ev := wireEvent()
			ev.Provider = provider
			ev.SameRepo = true // even set true, the provider gate wins
			outcomes, resolveErr := h.applyPRHeadConfig(ctx, ev, materials, wireCauseDetail(), "d", []byte("{}"))
			assertAllBaseNoFetch(t, outcomes, resolveErr, f, materials)
		})
	}
}

// applyPRHeadConfig returns structured outcomes and a TYPED resolution error, so
// the caller can fold them into the delivery's runs/status (and map a fetch
// failure to 503 vs a bad config to 422) instead of a misleading 202.
func TestApplyPRHeadConfig_OutcomesAndTypedResolveErr(t *testing.T) {
	t.Run("valid → created head outcome", func(t *testing.T) {
		h, _, f, pool, ctx, materials := seedWiring(t)
		f.files = []scm.RawFile{rawFile("build.yaml", "build")}
		outcomes, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "d", []byte("{}"))
		if resolveErr != nil {
			t.Fatalf("resolveErr = %v, want nil", resolveErr)
		}
		if len(outcomes) != 2 { // gov base + build head
			t.Fatalf("outcomes = %d, want 2", len(outcomes))
		}
		if src := configSource(t, pool, ctx, "build"); src != "pr_head" {
			t.Fatalf("build config_source = %q, want pr_head", src)
		}
	})
	t.Run("fetch error → ErrPRHeadFetch, repo blocked, base ran", func(t *testing.T) {
		h, _, f, pool, ctx, materials := seedWiring(t)
		f.err = errors.New("boom")
		outcomes, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "d", []byte("{}"))
		if !errors.Is(resolveErr, ErrPRHeadFetch) {
			t.Fatalf("resolveErr = %v, want ErrPRHeadFetch", resolveErr)
		}
		if n := runCount(t, pool, ctx, "build"); n != 0 {
			t.Fatalf("build runs = %d, want 0 (repo blocked)", n)
		}
		if n := runCount(t, pool, ctx, "gov"); n != 1 {
			t.Fatalf("gov runs = %d, want 1 (base ran base-first)", n)
		}
		if len(outcomes) != 1 { // just the gov base run
			t.Fatalf("outcomes = %d, want 1 (gov base)", len(outcomes))
		}
	})
	t.Run("invalid/missing config → ErrPRHeadConfigInvalid", func(t *testing.T) {
		h, _, f, _, ctx, materials := seedWiring(t)
		f.files = []scm.RawFile{} // build authorized but absent from head
		_, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "d", []byte("{}"))
		if !errors.Is(resolveErr, ErrPRHeadConfigInvalid) {
			t.Fatalf("resolveErr = %v, want ErrPRHeadConfigInvalid", resolveErr)
		}
	})
}

// Ambiguous binding: the clone URL is bound to >1 OPTED-IN project → the repo
// pipelines fail closed with ErrPRHeadBinding; system_managed still runs base; no
// fetch. (A second binding that is NOT opted in would leave exactly one trusted
// binding — not ambiguous — so demo2 must also enable the toggle.)
func TestApplyPRHeadConfig_AmbiguousBindingBlocksRepo(t *testing.T) {
	h, s, f, pool, ctx, materials := seedWiring(t)
	// A second project bound to the SAME url, ALSO opted in → 2 trusted bindings.
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo2", Name: "demo2",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: wireURL, DefaultBranch: wireBranch},
	}); err != nil {
		t.Fatalf("apply demo2: %v", err)
	}
	if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "demo2", true); err != nil {
		t.Fatalf("enable demo2 toggle: %v", err)
	}

	outcomes, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "d", []byte("{}"))

	if !errors.Is(resolveErr, ErrPRHeadBinding) {
		t.Fatalf("resolveErr = %v, want ErrPRHeadBinding", resolveErr)
	}
	if n := runCount(t, pool, ctx, "gov"); n != 1 {
		t.Fatalf("gov runs = %d, want 1 (system_managed still runs base)", n)
	}
	if n := runCount(t, pool, ctx, "build"); n != 0 {
		t.Fatalf("build runs = %d, want 0 (ambiguous binding blocks repo)", n)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1 (gov base)", len(outcomes))
	}
	if f.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (blocked before fetch)", f.calls)
	}
}

// A single UNTRUSTED extra binding on the same URL is NOT ambiguous — one trusted
// binding remains, so the head path still runs for the opted-in project.
func TestApplyPRHeadConfig_UntrustedSecondBindingNotAmbiguous(t *testing.T) {
	h, s, f, pool, ctx, materials := seedWiring(t)
	f.files = []scm.RawFile{rawFile("build.yaml", "build")}
	// demo2 shares the URL but does NOT opt in (default toggle false).
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo2", Name: "demo2",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: wireURL, DefaultBranch: wireBranch},
	}); err != nil {
		t.Fatalf("apply demo2: %v", err)
	}

	outcomes, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "d", []byte("{}"))
	if resolveErr != nil {
		t.Fatalf("resolveErr = %v, want nil (one trusted binding is unambiguous)", resolveErr)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2 (gov base + build head)", len(outcomes))
	}
	if src := configSource(t, pool, ctx, "build"); src != "pr_head" {
		t.Fatalf("build config_source = %q, want pr_head", src)
	}
}

// A MISSING binding (0 rows — e.g. a concurrent scm_source deletion between HMAC
// auth and dispatch) is distinct from "all toggles off": it fails the repo
// pipelines closed with ErrPRHeadBindingMissing (never a silent base run), while
// system_managed still runs base. No fetch.
func TestApplyPRHeadConfig_MissingBindingBlocksRepo(t *testing.T) {
	h, _, f, pool, ctx, materials := seedWiring(t)
	if _, err := pool.Exec(ctx, `DELETE FROM scm_sources`); err != nil {
		t.Fatalf("delete scm_sources: %v", err)
	}
	outcomes, resolveErr := h.applyPRHeadConfig(ctx, wireEvent(), materials, wireCauseDetail(), "d", []byte("{}"))
	if !errors.Is(resolveErr, ErrPRHeadBindingMissing) {
		t.Fatalf("resolveErr = %v, want ErrPRHeadBindingMissing", resolveErr)
	}
	if n := runCount(t, pool, ctx, "gov"); n != 1 {
		t.Fatalf("gov runs = %d, want 1 (system_managed still runs base)", n)
	}
	if n := runCount(t, pool, ctx, "build"); n != 0 {
		t.Fatalf("build runs = %d, want 0 (repo blocked, binding missing)", n)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1 (gov base)", len(outcomes))
	}
	if f.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (blocked before fetch)", f.calls)
	}
}
