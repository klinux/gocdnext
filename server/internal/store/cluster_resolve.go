package store

import (
	"context"
	"fmt"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// ResolveClusters is the apply-time validator for `cluster:` job
// references: each named cluster must exist in the registry, or the
// apply fails loudly citing the NAME (never a credential) so a typo or
// a deleted cluster is caught before a run is ever created.
//
// Authorization (allowed_projects) is deliberately NOT checked here:
// on a project's FIRST apply the project row may not exist yet, and the
// run's project is the authoritative subject anyway —
// ResolveClusterForDispatch enforces allowed_projects at dispatch
// (defense in depth). This pass is the early-feedback existence gate.
//
// Cached per apply: one existence probe per distinct cluster name.
func (s *Store) ResolveClusters(ctx context.Context, pipelines []*domain.Pipeline) error {
	checked := map[string]struct{}{}
	probe := func(pipeline, job, field, name string) error {
		if name == "" {
			return nil
		}
		if _, ok := checked[name]; ok {
			return nil
		}
		exists, err := s.q.ClusterExists(ctx, name)
		if err != nil {
			return fmt.Errorf("pipeline %q: job %q: check cluster %q: %w", pipeline, job, name, err)
		}
		if !exists {
			return fmt.Errorf("pipeline %q: job %q: %s %q is not registered", pipeline, job, field, name)
		}
		checked[name] = struct{}{}
		return nil
	}
	for _, p := range pipelines {
		for i := range p.Jobs {
			if err := probe(p.Name, p.Jobs[i].Name, "cluster", p.Jobs[i].Cluster); err != nil {
				return err
			}
			// A DECLARED deploy target names a cluster too. Without this a typo would
			// sail through apply and only surface at dispatch as the deliberately
			// collapsed "not found or not accessible" — losing the early, specific
			// feedback that is the whole point of an apply-time existence gate.
			if d := p.Jobs[i].Deploy; d != nil && d.Target != nil {
				if err := probe(p.Name, p.Jobs[i].Name, "deploy.target.cluster", d.Target.Cluster); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
