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

func rawFile(fname, name string) scm.RawFile {
	return scm.RawFile{Name: fname, Content: pipelineYAML(name)}
}

func authPipe(name string, sys bool) authorizedPipeline {
	return authorizedPipeline{Name: name, PipelineID: uuid.New(), MaterialID: uuid.New(), SystemManaged: sys}
}

func resolve(t *testing.T, f *fakeCfgFetcher, authorized []authorizedPipeline) ([]prHeadPlanEntry, error) {
	t.Helper()
	return resolvePRHeadPlan(context.Background(), f, store.SCMSource{Provider: "github"}, ".gocdnext", "headsha", authorized)
}

// Happy path: an authorized pipeline with a matching head definition yields one
// plan entry, and the config is fetched exactly once.
func TestResolvePRHeadPlan_Happy(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{rawFile("build.yaml", "build")}}
	plan, err := resolve(t, f, []authorizedPipeline{authPipe("build", false)})
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
	plan, err := resolve(t, f, []authorizedPipeline{authPipe("build", false), authPipe("deploy", false)})
	if err == nil {
		t.Fatal("expected an error when an authorized pipeline is absent from the head")
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil (no partial plan)", plan)
	}
}

// A pipeline that exists in the head but is NOT authorized by the base is
// ignored — it never registers or runs.
func TestResolvePRHeadPlan_NewInHeadIgnored(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{rawFile("build.yaml", "build"), rawFile("extra.yaml", "extra")}}
	plan, err := resolve(t, f, []authorizedPipeline{authPipe("build", false)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan) != 1 || plan[0].HeadDef.Name != "build" {
		t.Fatalf("plan = %+v, want only the authorized build entry (extra ignored)", plan)
	}
}

// A system_managed authorized pipeline is skipped — its definition stays
// server-owned, never sourced from the head.
func TestResolvePRHeadPlan_SystemManagedSkipped(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{rawFile("build.yaml", "build")}}
	plan, err := resolve(t, f, []authorizedPipeline{authPipe("build", true)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan) != 0 {
		t.Fatalf("plan = %+v, want empty (system_managed uses the server def)", plan)
	}
}

// A duplicate pipeline name across head files fails closed (via ParseFiles).
func TestResolvePRHeadPlan_DuplicateNameFailsClosed(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{rawFile("a.yaml", "build"), rawFile("b.yaml", "build")}}
	if _, err := resolve(t, f, []authorizedPipeline{authPipe("build", false)}); err == nil {
		t.Fatal("expected an error for a duplicate pipeline name")
	}
}

// Malformed YAML in the head fails closed at the parse step.
func TestResolvePRHeadPlan_ParseErrorFailsClosed(t *testing.T) {
	f := &fakeCfgFetcher{files: []scm.RawFile{{Name: "build.yaml", Content: "this: is: not: valid: pipeline"}}}
	if _, err := resolve(t, f, []authorizedPipeline{authPipe("build", false)}); err == nil {
		t.Fatal("expected a parse error for malformed config")
	}
}

// A fetch error fails closed with no plan (and no silent fallback to base).
func TestResolvePRHeadPlan_FetchErrorFailsClosed(t *testing.T) {
	f := &fakeCfgFetcher{err: errors.New("boom")}
	plan, err := resolve(t, f, []authorizedPipeline{authPipe("build", false)})
	if err == nil {
		t.Fatal("expected a fetch error")
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil", plan)
	}
}
