package webhook

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/configsync"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

var (
	// ErrPRHeadFetch wraps a config fetch / SCM failure — transient, maps to 503.
	ErrPRHeadFetch = errors.New("pr-head: config fetch failed")
	// ErrPRHeadConfigInvalid wraps a parse / declarative-target / envelope error
	// (bad or missing head config) — maps to 422.
	ErrPRHeadConfigInvalid = errors.New("pr-head: config invalid")
)

// authorizedPipeline is a base-authorized REPO pipeline the head config may
// drive. The set comes from the base (the project's registered pipelines matched
// by the PR's base ref), MINUS any system_managed ones: those are partitioned
// out by the caller (the wiring) and never reach the resolver, so a head that
// removes or breaks `.gocdnext/` can't suppress a mandatory server-owned
// pipeline — it keeps running on the base flow.
type authorizedPipeline struct {
	Name       string
	PipelineID uuid.UUID
	MaterialID uuid.UUID
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
// REPO pipelines. It writes NOTHING.
//
// The caller MUST have already partitioned out system_managed pipelines — they
// run on the base flow, never the head. With no repo pipelines to resolve this
// does NOT fetch, so a PR made entirely of system_managed work (or one whose
// head deleted `.gocdnext/`) never blocks the mandatory server-owned pipelines.
//
// Fails closed, with no partial plan:
//   - a nil fetcher or an empty headSHA (which would fetch the wrong ref) → error;
//   - any fetch / parse (incl. a duplicate pipeline name) / declarative-target
//     validation error → error;
//   - an authorized repo pipeline ABSENT from the head → error (never a silent
//     fallback to the base definition).
//
// A head pipeline NOT in the authorized set is IGNORED — a pipeline new in the
// head never registers or runs.
func resolvePRHeadPlan(
	ctx context.Context,
	fetcher ConfigFetcher,
	source store.SCMSource,
	configPath, headSHA string,
	authorized []authorizedPipeline,
) ([]prHeadPlanEntry, error) {
	// Nothing to resolve (e.g. a PR of only system_managed work) → no fetch, and
	// no dependency on the fetcher or head SHA either: a project that never
	// consults the head must not error on a missing fetcher or a malformed SHA.
	if len(authorized) == 0 {
		return nil, nil
	}
	if fetcher == nil {
		return nil, fmt.Errorf("pr-head: no config fetcher configured")
	}
	if headSHA == "" {
		return nil, fmt.Errorf("pr-head: empty head SHA")
	}

	files, err := fetcher.Fetch(ctx, source, headSHA, configPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPRHeadFetch, err)
	}
	// ParseFiles already rejects a duplicate pipeline name across files.
	pipelines, err := configsync.ParseFiles(files)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPRHeadConfigInvalid, err)
	}
	if err := configsync.ValidateDeclarativeTargets(pipelines); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPRHeadConfigInvalid, err)
	}

	headByName := make(map[string]*domain.Pipeline, len(pipelines))
	for _, p := range pipelines {
		headByName[p.Name] = p
	}

	plan := make([]prHeadPlanEntry, 0, len(authorized))
	for _, a := range authorized {
		hd, ok := headByName[a.Name]
		if !ok {
			return nil, fmt.Errorf(
				"%w: pipeline %q is authorized on the base but absent from the head config", ErrPRHeadConfigInvalid, a.Name)
		}
		plan = append(plan, prHeadPlanEntry{
			PipelineID: a.PipelineID,
			MaterialID: a.MaterialID,
			HeadDef:    *hd,
		})
	}
	return plan, nil
}
