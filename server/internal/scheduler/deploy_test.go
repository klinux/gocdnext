package scheduler_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// #39: BuildAssignment resolves a deploy job's version with the same
// needs.outputs + CI-var sources the env uses, and returns it as a
// DeployTarget for the dispatch path to record.

func TestBuildAssignment_DeployTarget_ResolvesVersionFromNeedsOutputs(t *testing.T) {
	def := domain.Pipeline{
		Jobs: []domain.Job{{
			Name:   "sync-prod",
			Needs:  []string{"build"},
			Tasks:  []domain.Task{{Plugin: &domain.PluginStep{Image: "ghcr.io/x/argocd:v1", Settings: map[string]string{}}}},
			Deploy: &domain.DeploySpec{Environment: "production", Version: "${{ needs.build.outputs.image-tag }}"},
		}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "sync-prod", Needs: []string{"build"}}
	needs := scheduler.NeedsOutputs{"build": {"image-tag": "1.42.abc"}}

	_, target, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, needs, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if target == nil {
		t.Fatal("DeployTarget nil for a deploy job")
	}
	if target.Environment != "production" || target.Version != "1.42.abc" {
		t.Fatalf("target = %+v, want {production 1.42.abc}", target)
	}
}

func TestBuildAssignment_DeployTarget_DefaultsToCommitShortSha(t *testing.T) {
	def := domain.Pipeline{
		Jobs: []domain.Job{{
			Name:   "ship",
			Tasks:  []domain.Task{{Script: "kubectl apply -f ."}},
			Deploy: &domain.DeploySpec{Environment: "staging"}, // version omitted
		}},
	}
	defJSON, _ := json.Marshal(def)
	revs, _ := json.Marshal(map[string]map[string]string{
		"git:repo": {"revision": "abcdef1234567890", "branch": "main"},
	})
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON, Revisions: revs}
	job := store.DispatchableJob{ID: uuid.New(), Name: "ship"}

	_, target, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if target == nil {
		t.Fatal("DeployTarget nil for a deploy job")
	}
	if target.Version != "abcdef12" { // shortSHALen = 8
		t.Fatalf("version = %q, want abcdef12 (commit short sha default)", target.Version)
	}
}

func TestBuildAssignment_DeployTarget_ShellStyleCIVars(t *testing.T) {
	// Parity with plugin settings: `1.${CI_RUN_COUNTER}.${CI_COMMIT_SHORT_SHA}`
	// resolves via the soft shell-style pass after the strict pass.
	def := domain.Pipeline{
		Jobs: []domain.Job{{
			Name:   "ship",
			Tasks:  []domain.Task{{Script: "true"}},
			Deploy: &domain.DeploySpec{Environment: "production", Version: "1.${CI_RUN_COUNTER}.${CI_COMMIT_SHORT_SHA}"},
		}},
	}
	defJSON, _ := json.Marshal(def)
	revs, _ := json.Marshal(map[string]map[string]string{
		"git:repo": {"revision": "abcdef1234567890", "branch": "main"},
	})
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Counter: 7, Definition: defJSON, Revisions: revs}
	job := store.DispatchableJob{ID: uuid.New(), Name: "ship"}

	_, target, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if target == nil || target.Version != "1.7.abcdef12" {
		t.Fatalf("version = %+v, want 1.7.abcdef12", target)
	}
}

func TestBuildAssignment_DeployTarget_EmptyVersionIsTerminal(t *testing.T) {
	// version omitted AND no git revision (manual run, no material) →
	// terminal config error, not a blank version recorded forever.
	def := domain.Pipeline{
		Jobs: []domain.Job{{
			Name:   "ship",
			Tasks:  []domain.Task{{Script: "true"}},
			Deploy: &domain.DeploySpec{Environment: "production"}, // no version
		}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON} // no Revisions
	job := store.DispatchableJob{ID: uuid.New(), Name: "ship"}

	_, _, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil)
	if !errors.Is(err, scheduler.ErrDeployVersionEmpty) {
		t.Fatalf("err = %v, want ErrDeployVersionEmpty", err)
	}
}

func TestBuildAssignment_DeployTarget_UnresolvableCIVarIsTerminal(t *testing.T) {
	// A `${{ CI_* }}` the parser accepted by shape but this run
	// doesn't carry (e.g. CI_TAG_NAME on a non-tag run) must be a
	// TERMINAL config error, not a forever-retry. The parser can't
	// catch it (which CI vars exist is per-run), so the dispatch path
	// wraps it as ErrDeployVersionUnresolved.
	def := domain.Pipeline{
		Jobs: []domain.Job{{
			Name:   "ship",
			Tasks:  []domain.Task{{Script: "true"}},
			Deploy: &domain.DeploySpec{Environment: "production", Version: "${{ CI_TAG_NAME }}"},
		}},
	}
	defJSON, _ := json.Marshal(def)
	// Run with a branch revision (not a tag) → CI_TAG_NAME absent.
	revs, _ := json.Marshal(map[string]map[string]string{
		"git:repo": {"revision": "abcdef1234567890", "branch": "main"},
	})
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON, Revisions: revs}
	job := store.DispatchableJob{ID: uuid.New(), Name: "ship"}

	_, _, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil)
	if !errors.Is(err, scheduler.ErrDeployVersionUnresolved) {
		t.Fatalf("err = %v, want ErrDeployVersionUnresolved", err)
	}
}

func TestBuildAssignment_DeployTarget_UnresolvedShellCIVarIsTerminal(t *testing.T) {
	// Shell-style ${CI_TAG_NAME} on a non-tag run: the soft pass would
	// leave it literal, but deploy.version is persisted metadata with
	// no later shell — recording a literal ${CI_TAG_NAME} would be a
	// lie. It must terminalise, same as the strict form.
	revs, _ := json.Marshal(map[string]map[string]string{
		"git:repo": {"revision": "abcdef1234567890", "branch": "main"},
	})
	// Plain ${CI_TAG_NAME} and the shell modifier forms
	// (${CI_TAG_NAME:-dev}, ${CI_TAG_NAME?missing}) all leave a
	// ${CI_...} literal after the soft pass — none may persist.
	for _, version := range []string{"${CI_TAG_NAME}", "${CI_TAG_NAME:-dev}", "v${CI_TAG_NAME?missing}"} {
		def := domain.Pipeline{
			Jobs: []domain.Job{{
				Name:   "ship",
				Tasks:  []domain.Task{{Script: "true"}},
				Deploy: &domain.DeploySpec{Environment: "production", Version: version},
			}},
		}
		defJSON, _ := json.Marshal(def)
		run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON, Revisions: revs}
		job := store.DispatchableJob{ID: uuid.New(), Name: "ship"}

		_, _, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil)
		if !errors.Is(err, scheduler.ErrDeployVersionUnresolved) {
			t.Fatalf("version %q: err = %v, want ErrDeployVersionUnresolved", version, err)
		}
	}
}

func TestBuildAssignment_NoDeployTargetForPlainJob(t *testing.T) {
	def := domain.Pipeline{Jobs: []domain.Job{{
		Name:  "test",
		Tasks: []domain.Task{{Script: "go test ./..."}},
	}}}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "test"}

	_, target, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if target != nil {
		t.Fatalf("plain job produced a DeployTarget: %+v", target)
	}
}
