package webhook

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/configsync"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// authorizedPipeline is a base-authorized pipeline the head config may drive.
// The set comes from the base (the project's registered pipelines matched by the
// PR's base ref) — the head can only ever run pipelines the base already knows.
type authorizedPipeline struct {
	Name          string
	PipelineID    uuid.UUID
	MaterialID    uuid.UUID
	SystemManaged bool
}

// prHeadPlanEntry is one authorised materialisation: the head definition to run
// for a base pipeline, keyed to the material CreatePRHeadRun anchors on.
type prHeadPlanEntry struct {
	PipelineID uuid.UUID
	MaterialID uuid.UUID
	HeadDef    domain.Pipeline
}

// resolvePRHeadPlan fetches + parses + validates the head `.gocdnext/` ONCE at
// headSHA (one fetch per (source, configPath, headSHA) tuple — the caller groups
// materials by that tuple), then builds the plan against the base-authorized
// set. It writes NOTHING.
//
// Fails closed, with no partial plan:
//   - any fetch / parse (incl. a duplicate pipeline name) / declarative-target
//     validation error → error;
//   - an authorized, non-system-managed pipeline ABSENT from the head → error
//     (never a silent fallback to the base definition).
//
// A head pipeline NOT in the authorized set is IGNORED (a pipeline new in the
// head never registers or runs). A system_managed authorized pipeline is SKIPPED
// (its definition is server-owned, never sourced from a PR head).
func resolvePRHeadPlan(
	ctx context.Context,
	fetcher ConfigFetcher,
	source store.SCMSource,
	configPath, headSHA string,
	authorized []authorizedPipeline,
) ([]prHeadPlanEntry, error) {
	files, err := fetcher.Fetch(ctx, source, headSHA, configPath)
	if err != nil {
		return nil, fmt.Errorf("pr-head: fetch config: %w", err)
	}
	// ParseFiles already rejects a duplicate pipeline name across files.
	pipelines, err := configsync.ParseFiles(files)
	if err != nil {
		return nil, fmt.Errorf("pr-head: parse config: %w", err)
	}
	if err := configsync.ValidateDeclarativeTargets(pipelines); err != nil {
		return nil, fmt.Errorf("pr-head: validate config: %w", err)
	}

	headByName := make(map[string]*domain.Pipeline, len(pipelines))
	for _, p := range pipelines {
		headByName[p.Name] = p
	}

	plan := make([]prHeadPlanEntry, 0, len(authorized))
	for _, a := range authorized {
		if a.SystemManaged {
			continue // server-owned definition, never sourced from a PR head
		}
		hd, ok := headByName[a.Name]
		if !ok {
			return nil, fmt.Errorf(
				"pr-head: pipeline %q is authorized on the base but absent from the head config", a.Name)
		}
		plan = append(plan, prHeadPlanEntry{
			PipelineID: a.PipelineID,
			MaterialID: a.MaterialID,
			HeadDef:    *hd,
		})
	}
	return plan, nil
}
