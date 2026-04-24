package parser

import (
	"fmt"
	"strings"
)

// hiddenJobPrefix marks "template-only" jobs that exist solely to
// be extended, never to run. Matches GitLab CI convention; a job
// named `.base-build` gets stripped from the materialized job
// list after extends resolution.
const hiddenJobPrefix = "."

// resolveExtends walks every entry in `jobs`, merging each job's
// `extends:` chain into a single flattened JobDef. The returned
// map contains only jobs that should actually run — hidden
// template jobs (names starting with "." ) are dropped.
//
// Merge rules per field — they mirror GitLab CI's "child wins on
// scalar, child replaces on list, map keys overlay" model so an
// operator's muscle memory transfers cleanly:
//
//   - Scalars (stage, image, extends, timeout, uses, retry, docker):
//     child's value wins when set; otherwise falls back to parent.
//   - Lists (script, needs, secrets, tags, cache, needs_artifacts):
//     child replaces parent entirely when non-nil. This keeps the
//     model simple — concatenating lists would force users to
//     reason about "why is this extra -foo flag showing up?" on
//     every inherited job.
//   - Maps (settings, with, variables): parent keys carry over;
//     child keys overlay by key (child-set wins, siblings from
//     parent stay). Same semantics as `spread` in most config
//     systems.
//   - `rules`, `when`, `approval`, `artifacts`, `parallel`: pointer
//     or struct child overrides the whole thing; they're small
//     enough that "child replaces" is clearer than deep-merge.
//
// Chains are supported: A extends B extends C merges C→B→A.
// Cycles return an error rather than looping.
func resolveExtends(jobs map[string]JobDef) (map[string]JobDef, error) {
	out := make(map[string]JobDef, len(jobs))
	resolving := map[string]bool{}
	cache := map[string]JobDef{}

	var resolve func(name string) (JobDef, error)
	resolve = func(name string) (JobDef, error) {
		if cached, ok := cache[name]; ok {
			return cached, nil
		}
		if resolving[name] {
			return JobDef{}, fmt.Errorf("job %q: extends cycle (chain comes back to itself)", name)
		}
		jd, ok := jobs[name]
		if !ok {
			return JobDef{}, fmt.Errorf("extends target %q is not defined anywhere in this pipeline", name)
		}
		if jd.Extends == "" {
			cache[name] = jd
			return jd, nil
		}
		resolving[name] = true
		parent, err := resolve(jd.Extends)
		delete(resolving, name)
		if err != nil {
			return JobDef{}, fmt.Errorf("job %q: %w", name, err)
		}
		merged := mergeJobDef(parent, jd)
		// The merged jobDef keeps its own name identity; clear
		// `Extends` on the cached copy so a consumer never thinks
		// the chain is still open.
		merged.Extends = ""
		cache[name] = merged
		return merged, nil
	}

	for name := range jobs {
		merged, err := resolve(name)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(name, hiddenJobPrefix) {
			// Template job — exists only so other jobs can extend
			// it. Never materializes.
			continue
		}
		out[name] = merged
	}
	return out, nil
}

// mergeJobDef produces (child-wins-where-set) merge of base + child.
// Called with the fully resolved base — callers that see a chain
// (A extends B extends C) must have resolved C→B before calling
// this with (B, A).
func mergeJobDef(base, child JobDef) JobDef {
	out := base

	// --- scalars: child wins when set ---
	if child.Stage != "" {
		out.Stage = child.Stage
	}
	if child.Image != "" {
		out.Image = child.Image
	}
	if child.Uses != "" {
		out.Uses = child.Uses
	}
	if child.Timeout != "" {
		out.Timeout = child.Timeout
	}
	if child.Retry != 0 {
		out.Retry = child.Retry
	}
	if child.Docker {
		out.Docker = true
	}
	// Extends is intentionally carried over verbatim so the
	// caller can see the chain; resolveExtends clears it after
	// the full merge.
	if child.Extends != "" {
		out.Extends = child.Extends
	}

	// --- lists: child replaces when non-nil ---
	if child.Script != nil {
		out.Script = append([]string(nil), child.Script...)
	}
	if child.Needs != nil {
		out.Needs = append([]string(nil), child.Needs...)
	}
	if child.Secrets != nil {
		out.Secrets = append([]string(nil), child.Secrets...)
	}
	if child.Tags != nil {
		out.Tags = append([]string(nil), child.Tags...)
	}
	if child.Cache != nil {
		out.Cache = append([]CacheSpec(nil), child.Cache...)
	}
	if child.NeedsArtifacts != nil {
		out.NeedsArtifacts = append([]NeedsArtifactDef(nil), child.NeedsArtifacts...)
	}
	if child.Rules != nil {
		out.Rules = append([]RuleDef(nil), child.Rules...)
	}

	// --- maps: key-level overlay (parent keys stay, child wins on conflict) ---
	out.Settings = overlayStrMap(base.Settings, child.Settings)
	out.With = overlayStrMap(base.With, child.With)
	out.Variables = overlayStrMap(base.Variables, child.Variables)

	// --- pointers/structs: child wins whole ---
	if child.Artifacts != nil {
		out.Artifacts = child.Artifacts
	}
	if child.Parallel != nil {
		out.Parallel = child.Parallel
	}
	if child.When != nil {
		out.When = child.When
	}
	if child.Approval != nil {
		out.Approval = child.Approval
	}

	return out
}

func overlayStrMap(base, child map[string]string) map[string]string {
	if base == nil && child == nil {
		return nil
	}
	out := make(map[string]string, len(base)+len(child))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range child {
		out[k] = v
	}
	return out
}
