package webhook

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/scm"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// fakeCfgFetcher is a configsync.Fetcher that returns canned files, so the
// resolver's fetch/parse/validate/plan logic is exercised without any network.
type fakeCfgFetcher struct {
	files []scm.RawFile
	err   error
	calls int
}

func (f *fakeCfgFetcher) Fetch(_ context.Context, _ store.SCMSource, _, _ string) ([]scm.RawFile, error) {
	f.calls++
	return f.files, f.err
}

func (f *fakeCfgFetcher) HeadSHA(_ context.Context, _ store.SCMSource, _ string) (string, error) {
	return "", nil
}

func pipelineYAML(name string) string {
	return "name: " + name + "\n" +
		"stages: [build]\n" +
		"jobs:\n" +
		"  compile:\n" +
		"    stage: build\n" +
		"    image: alpine:3.20\n" +
		"    script:\n" +
		"      - make\n"
}

// deployYAML declares a single deploy job targeting `env` on `cluster` — two of
// these with the same env but different clusters conflict at
// ValidateDeclarativeTargets.
func deployYAML(name, env, cluster string) string {
	return "name: " + name + "\n" +
		"stages: [deploy]\n" +
		"jobs:\n" +
		"  release:\n" +
		"    stage: deploy\n" +
		"    uses: ghcr.io/klinux/gocdnext-plugin-argocd@v1\n" +
		"    deploy:\n" +
		"      environment: " + env + "\n" +
		"      target:\n" +
		"        cluster: " + cluster + "\n" +
		"        application: app\n"
}

func rawFile(fname, name string) scm.RawFile {
	return scm.RawFile{Name: fname, Content: pipelineYAML(name)}
}

func authPipe(name string) authorizedPipeline {
	return authorizedPipeline{Name: name, PipelineID: uuid.New(), MaterialID: uuid.New()}
}

func resolve(t *testing.T, f *fakeCfgFetcher, authorized []authorizedPipeline) ([]prHeadPlanEntry, error) {
	t.Helper()
	return resolvePRHeadPlan(context.Background(), f, store.SCMSource{Provider: "github"}, ".gocdnext", "headsha", authorized)
}

// Happy path: an authorized pipeline with a matching head definition yields one
// plan entry, and the config is fetched exactly once.
func TestResolvePRHeadPlan_Happy(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{rawFile("build.yaml", "build")}}
	plan, err := resolve(t, f, []authorizedPipeline{authPipe("build")})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan) != 1 || plan[0].HeadDef.Name != "build" {
		t.Fatalf("plan = %+v, want one build entry", plan)
	}
	if f.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1 (one fetch per tuple)", f.calls)
	}
}

// An authorized pipeline absent from the head fails closed — no partial plan.
func TestResolvePRHeadPlan_MissingPipelineFailsClosed(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{rawFile("build.yaml", "build")}}
	plan, err := resolve(t, f, []authorizedPipeline{authPipe("build"), authPipe("deploy")})
	if err == nil {
		t.Fatal("expected an error when an authorized pipeline is absent from the head")
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil (no partial plan)", plan)
	}
}

// A pipeline in the head but NOT authorized by the base is ignored.
func TestResolvePRHeadPlan_NewInHeadIgnored(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{rawFile("build.yaml", "build"), rawFile("extra.yaml", "extra")}}
	plan, err := resolve(t, f, []authorizedPipeline{authPipe("build")})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan) != 1 || plan[0].HeadDef.Name != "build" {
		t.Fatalf("plan = %+v, want only the authorized build entry (extra ignored)", plan)
	}
}

// No repo pipelines to resolve (e.g. a PR of only system_managed work, which the
// caller partitions out) → NO fetch, so a head that deleted `.gocdnext/` can't
// block the mandatory server-owned pipelines.
func TestResolvePRHeadPlan_EmptyAuthorizedNoFetch(t *testing.T) {
	// Empty short-circuits BEFORE the fetcher/SHA guards: a system_managed-only
	// project never consults the head, so even a nil fetcher and an empty SHA
	// must not error.
	if plan, err := resolvePRHeadPlan(context.Background(), nil, store.SCMSource{}, ".gocdnext", "", nil); err != nil || plan != nil {
		t.Fatalf("resolve(empty, nil fetcher, empty sha) = (%+v, %v), want (nil, nil)", plan, err)
	}
	// And with a real fetcher present it is still never called.
	f := &fakeCfgFetcher{err: errors.New("should not be called")}
	if plan, err := resolve(t, f, nil); err != nil || plan != nil {
		t.Fatalf("resolve(empty) = (%+v, %v), want (nil, nil)", plan, err)
	}
	if f.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (no repo pipelines → no fetch)", f.calls)
	}
}

// Input guards: a nil fetcher or an empty head SHA is rejected before any fetch.
func TestResolvePRHeadPlan_InputGuards(t *testing.T) {
	if _, err := resolvePRHeadPlan(context.Background(), nil, store.SCMSource{}, ".gocdnext", "sha", []authorizedPipeline{authPipe("build")}); err == nil {
		t.Fatal("nil fetcher: expected an error")
	}
	f := &fakeCfgFetcher{files: []scm.RawFile{rawFile("build.yaml", "build")}}
	if _, err := resolvePRHeadPlan(context.Background(), f, store.SCMSource{}, ".gocdnext", "", []authorizedPipeline{authPipe("build")}); err == nil {
		t.Fatal("empty head SHA: expected an error")
	}
	if f.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 (guards reject before fetch)", f.calls)
	}
}

// A duplicate pipeline name across head files fails closed (via ParseFiles).
func TestResolvePRHeadPlan_DuplicateNameFailsClosed(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{rawFile("a.yaml", "build"), rawFile("b.yaml", "build")}}
	if _, err := resolve(t, f, []authorizedPipeline{authPipe("build")}); err == nil {
		t.Fatal("expected an error for a duplicate pipeline name")
	}
}

// Malformed YAML in the head fails closed at the parse step.
func TestResolvePRHeadPlan_ParseErrorFailsClosed(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{{Name: "build.yaml", Content: "this: is: not: valid: pipeline"}}}
	if _, err := resolve(t, f, []authorizedPipeline{authPipe("build")}); err == nil {
		t.Fatal("expected a parse error for malformed config")
	}
}

// Conflicting declarative deploy targets for one environment fail closed at
// ValidateDeclarativeTargets.
func TestResolvePRHeadPlan_ValidateTargetsFailsClosed(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{
		{Name: "a.yaml", Content: deployYAML("build", "prod", "clusterA")},
		{Name: "b.yaml", Content: deployYAML("other", "prod", "clusterB")},
	}}
	if _, err := resolve(t, f, []authorizedPipeline{authPipe("build")}); err == nil {
		t.Fatal("expected a ValidateDeclarativeTargets conflict error")
	}
}

// A fetch error fails closed with no plan (and no silent fallback to base).
func TestResolvePRHeadPlan_FetchErrorFailsClosed(t *testing.T) {
	f := &fakeCfgFetcher{err: errors.New("boom")}
	plan, err := resolve(t, f, []authorizedPipeline{authPipe("build")})
	if err == nil {
		t.Fatal("expected a fetch error")
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil", plan)
	}
}
