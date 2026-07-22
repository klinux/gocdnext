package configsync_test

import (
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/configsync"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func pipelineWithTarget(name, job, env string, t *domain.DeployTargetSpec) *domain.Pipeline {
	return &domain.Pipeline{
		Name: name,
		Jobs: []domain.Job{{
			Name:   job,
			Deploy: &domain.DeploySpec{Environment: env, Target: t},
		}},
	}
}

func TestValidateDeclarativeTargets(t *testing.T) {
	full := func() *domain.DeployTargetSpec {
		return &domain.DeployTargetSpec{
			Cluster: "prod-hub", Application: "shop", Namespace: "argocd", SyncMode: "trigger",
		}
	}

	t.Run("no declarations is fine", func(t *testing.T) {
		p := &domain.Pipeline{Name: "a", Jobs: []domain.Job{{Name: "ship",
			Deploy: &domain.DeploySpec{Environment: "prod"}}}}
		if err := configsync.ValidateDeclarativeTargets([]*domain.Pipeline{p}); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("identical declarations across pipelines are fine", func(t *testing.T) {
		ps := []*domain.Pipeline{
			pipelineWithTarget("a", "ship", "prod", full()),
			pipelineWithTarget("b", "ship", "prod", full()),
		}
		if err := configsync.ValidateDeclarativeTargets(ps); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	// The compare must be semantic, not textual: an omitted namespace IS argocd, and
	// rejecting that pair would be a false conflict on identical intent.
	t.Run("omitted namespace equals the explicit default", func(t *testing.T) {
		bare := full()
		bare.Namespace = ""
		ps := []*domain.Pipeline{
			pipelineWithTarget("a", "ship", "prod", full()),
			pipelineWithTarget("b", "ship", "prod", bare),
		}
		if err := configsync.ValidateDeclarativeTargets(ps); err != nil {
			t.Fatalf("false conflict on an omitted namespace: %v", err)
		}
	})

	// A target is 1:1 with an environment, so two different declarations would make the
	// registered row flip per dispatch — which Application a deploy hits would depend on
	// scheduling order.
	t.Run("conflicting declarations are rejected and name both origins", func(t *testing.T) {
		other := full()
		other.Application = "shop-canary"
		ps := []*domain.Pipeline{
			pipelineWithTarget("release", "ship", "prod", full()),
			pipelineWithTarget("hotfix", "deploy", "prod", other),
		}
		err := configsync.ValidateDeclarativeTargets(ps)
		if err == nil {
			t.Fatal("expected a conflict")
		}
		for _, want := range []string{"prod", "release/ship", "hotfix/deploy"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q — an operator needs to know where to look", err, want)
			}
		}
	})

	t.Run("conflict within a single pipeline is caught too", func(t *testing.T) {
		other := full()
		other.SyncMode = "observe"
		p := &domain.Pipeline{Name: "release", Jobs: []domain.Job{
			{Name: "a", Deploy: &domain.DeploySpec{Environment: "prod", Target: full()}},
			{Name: "b", Deploy: &domain.DeploySpec{Environment: "prod", Target: other}},
		}}
		if err := configsync.ValidateDeclarativeTargets([]*domain.Pipeline{p}); err == nil {
			t.Fatal("expected a conflict")
		}
	})

	t.Run("different environments never conflict", func(t *testing.T) {
		other := full()
		other.Application = "shop-staging"
		ps := []*domain.Pipeline{
			pipelineWithTarget("a", "ship", "prod", full()),
			pipelineWithTarget("a2", "ship", "staging", other),
		}
		if err := configsync.ValidateDeclarativeTargets(ps); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}
