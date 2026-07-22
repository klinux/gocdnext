package configsync

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// defaultTargetNamespace mirrors deploy.NormalizeNamespace's default. It is applied
// here ONLY to compare declarations; the write boundary still owns the stored value.
const defaultTargetNamespace = "argocd"

// ValidateDeclarativeTargets rejects an apply whose pipelines declare CONFLICTING
// deploy targets for one environment.
//
// A deploy target is 1:1 with an environment (UNIQUE (environment_id)), but nothing
// stops two jobs — or two pipelines — from declaring different ones for the same
// `deploy.environment`. Left alone that becomes "last dispatch wins": the registered
// target would flip between runs, and which Application a deploy hits would depend on
// scheduling order.
//
// It runs PRE-PERSIST, beside ResolveClusters, for two reasons: it needs no DB, and the
// handlers map a generic store error to 500 — burying this inside ApplyProject would
// report a config conflict as a server fault. It is sound at this layer because
// ApplyProject receives the project's WHOLE wanted set (it deletes pipelines absent
// from it), so every declaration is visible here.
func ValidateDeclarativeTargets(pipelines []*domain.Pipeline) error {
	type decl struct {
		spec   domain.DeployTargetSpec
		origin string // pipeline/job, for the message
	}
	seen := map[string]decl{}

	for _, p := range pipelines {
		for i := range p.Jobs {
			d := p.Jobs[i].Deploy
			if d == nil || d.Target == nil {
				continue
			}
			env := strings.TrimSpace(d.Environment)
			got := decl{
				spec:   normalizeTargetSpec(*d.Target),
				origin: fmt.Sprintf("%s/%s", p.Name, p.Jobs[i].Name),
			}
			prev, ok := seen[env]
			if !ok {
				seen[env] = got
				continue
			}
			// Compare NORMALIZED specs: two declarations that differ only by an
			// omitted `namespace` (vs a spelled-out "argocd") are the same target,
			// and rejecting them would be a false conflict.
			if prev.spec != got.spec {
				a, b := prev.origin, got.origin
				if b < a {
					a, b = b, a // stable message regardless of map/slice order
				}
				return fmt.Errorf(
					"environment %q has conflicting deploy.target declarations in %s and %s — "+
						"a deploy target is 1:1 with an environment, so they must be identical or only one job may declare it",
					env, a, b)
			}
		}
	}
	return nil
}

// normalizeTargetSpec applies the comparison-time defaults so equality means "the same
// target", not "the same characters".
func normalizeTargetSpec(t domain.DeployTargetSpec) domain.DeployTargetSpec {
	ns := strings.TrimSpace(t.Namespace)
	if ns == "" {
		ns = defaultTargetNamespace
	}
	return domain.DeployTargetSpec{
		Cluster:     strings.TrimSpace(t.Cluster),
		Application: strings.TrimSpace(t.Application),
		Namespace:   ns,
		SyncMode:    strings.TrimSpace(t.SyncMode),
	}
}

// DeclaredTargetEnvironments lists the environments a set of pipelines declares a target
// for, sorted. Used by tests and diagnostics; keeps the traversal in one place.
func DeclaredTargetEnvironments(pipelines []*domain.Pipeline) []string {
	set := map[string]struct{}{}
	for _, p := range pipelines {
		for i := range p.Jobs {
			if d := p.Jobs[i].Deploy; d != nil && d.Target != nil {
				set[strings.TrimSpace(d.Environment)] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
