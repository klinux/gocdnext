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
	for _, p := range pipelines {
		for i := range p.Jobs {
			name := p.Jobs[i].Cluster
			if name == "" {
				continue
			}
			if _, ok := checked[name]; ok {
				continue
			}
			exists, err := s.q.ClusterExists(ctx, name)
			if err != nil {
				return fmt.Errorf("pipeline %q: job %q: check cluster %q: %w", p.Name, p.Jobs[i].Name, name, err)
			}
			if !exists {
				return fmt.Errorf("pipeline %q: job %q: cluster %q is not registered", p.Name, p.Jobs[i].Name, name)
			}
			checked[name] = struct{}{}
		}
	}
	return nil
}
