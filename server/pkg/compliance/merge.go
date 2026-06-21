// Package compliance implements framework-scoped, enforced pipeline policies —
// the gocdnext equivalent of GitLab CI's compliance pipelines. An admin defines
// policies (pipeline config authored in the normal pipeline YAML schema) that
// target compliance frameworks; the server MERGES them into a project's
// effective pipeline definition at apply time, so mandatory jobs / approval
// gates run on every targeted project and repo authors cannot remove them.
//
// This file holds the pure merge engine: no DB, no I/O. ApplyPolicies is a
// deterministic function of (raw pipeline, policies) and is exhaustively unit
// tested. The store layer compiles policies, resolves which apply to a project,
// and calls ApplyPolicies; nothing here touches global state.
package compliance

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
	"github.com/gocdnext/gocdnext/server/pkg/parser"
)

const (
	// ModeInject appends a policy's stages/jobs to the project pipeline.
	ModeInject = "inject"
	// ModeOverride replaces the project's stages/jobs with the policy's.
	ModeOverride = "override"

	// ReservedPrefix namespaces every stage and job a policy contributes.
	// Repo YAML may not use it (enforced by RejectReservedNames at apply
	// time), which makes injected jobs impossible to shadow or remove and
	// keeps them clearly attributable in the UI and logs. Underscore-led to
	// match the existing reserved names (`_notifications`, `_notify_<i>`) and
	// to avoid ':' which is the OIDC subject-claim separator.
	ReservedPrefix = "_compliance_"
)

// Policy is a compiled, ready-to-merge compliance policy. Pipeline holds the
// parsed stages+jobs (all reserved-prefixed). PositionBefore/PositionAfter
// anchor inject-mode stages relative to an existing project stage; both empty
// means prepend (compliance runs first). They are ignored in override mode.
type Policy struct {
	Name           string
	Mode           string
	Priority       int
	PositionBefore string
	PositionAfter  string
	Pipeline       domain.Pipeline
}

// ApplyPolicies returns the effective pipeline: raw with every policy merged in
// priority order (ties broken by name, deterministic). The input raw is never
// mutated — callers store the result as the effective definition.
//
// Defence in depth: the raw side is first STRIPPED of any reserved-prefixed
// stage/job before merging. New repo configs are rejected loudly at apply time
// (RejectReservedNames), but legacy rows — e.g. a pre-feature pipeline whose
// definition_raw was backfilled by migration 00052, or a recompute reading such
// a row — could still carry a `_compliance_*` name that would shadow a policy
// job at dispatch (findJob picks the first match). Stripping here guarantees the
// reserved namespace is owned exclusively by policies, on every path.
func ApplyPolicies(raw domain.Pipeline, policies []Policy) domain.Pipeline {
	out := raw
	// No applicable policies → return the repo pipeline unchanged (copied).
	// We deliberately do NOT strip the reserved namespace here: with nothing
	// to shadow, stripping would be a silent change to a non-governed
	// project's definition. New repo configs using the prefix are still
	// rejected loudly at apply time (RejectReservedNames).
	if len(policies) == 0 {
		out.Stages = append([]string(nil), raw.Stages...)
		out.Jobs = append([]domain.Job(nil), raw.Jobs...)
		return out
	}
	// Governed: strip any reserved-prefixed repo entries so they can't shadow
	// a policy job (defends the legacy-backfill / recompute path).
	out.Stages = stripReservedStages(raw.Stages)
	out.Jobs = stripReservedJobs(raw.Jobs)

	ordered := append([]Policy(nil), policies...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].Name < ordered[j].Name
	})

	for _, p := range ordered {
		switch p.Mode {
		case ModeOverride:
			out.Stages = append([]string(nil), p.Pipeline.Stages...)
			out.Jobs = append([]domain.Job(nil), p.Pipeline.Jobs...)
		default: // inject
			out.Stages = insertStages(out.Stages, p.Pipeline.Stages, p.PositionBefore, p.PositionAfter)
			out.Jobs = appendUniqueJobs(out.Jobs, p.Pipeline.Jobs)
		}
	}
	return out
}

// stripReservedStages / stripReservedJobs drop reserved-prefixed entries from a
// repo-side pipeline so they can never shadow policy-owned names.
func stripReservedStages(stages []string) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		if !strings.HasPrefix(s, ReservedPrefix) {
			out = append(out, s)
		}
	}
	return out
}

func stripReservedJobs(jobs []domain.Job) []domain.Job {
	out := make([]domain.Job, 0, len(jobs))
	for _, j := range jobs {
		if !strings.HasPrefix(j.Name, ReservedPrefix) {
			out = append(out, j)
		}
	}
	return out
}

// appendUniqueJobs appends jobs whose names aren't already present, keeping the
// first occurrence. Policy-policy name collisions are blocked at create/update
// (store), so this is a deterministic last line of defence against duplicate
// job_runs at materialisation.
func appendUniqueJobs(existing, add []domain.Job) []domain.Job {
	seen := make(map[string]struct{}, len(existing)+len(add))
	for _, j := range existing {
		seen[j.Name] = struct{}{}
	}
	out := existing
	for _, j := range add {
		if _, dup := seen[j.Name]; dup {
			continue
		}
		seen[j.Name] = struct{}{}
		out = append(out, j)
	}
	return out
}

// insertStages splices the policy's new stages into base at the anchored
// position, skipping any that already exist (idempotent re-apply). Anchor: if
// `before` names an existing stage, insert immediately before it; else if
// `after` does, insert immediately after it; else prepend.
func insertStages(base, add []string, before, after string) []string {
	existing := make(map[string]bool, len(base))
	for _, s := range base {
		existing[s] = true
	}
	fresh := make([]string, 0, len(add))
	for _, s := range add {
		if !existing[s] {
			fresh = append(fresh, s)
			existing[s] = true // guard against duplicates within `add`
		}
	}
	if len(fresh) == 0 {
		return base
	}

	idx := 0 // default: prepend
	if i := indexOf(base, before); before != "" && i >= 0 {
		idx = i
	} else if i := indexOf(base, after); after != "" && i >= 0 {
		idx = i + 1
	}

	out := make([]string, 0, len(base)+len(fresh))
	out = append(out, base[:idx]...)
	out = append(out, fresh...)
	out = append(out, base[idx:]...)
	return out
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// CompilePolicy parses a policy's YAML (the normal pipeline schema: stages +
// jobs, jobs may carry approval gates) and validates that every stage and job
// name is reserved-prefixed. The returned pipeline is what gets stored as the
// policy's compiled `config` and later fed to ApplyPolicies.
func CompilePolicy(yamlSrc string) (domain.Pipeline, error) {
	p, err := parser.ParseNamed(strings.NewReader(yamlSrc), "", "policy")
	if err != nil {
		return domain.Pipeline{}, fmt.Errorf("compliance: parse policy config: %w", err)
	}
	if err := ValidatePolicyNames(*p); err != nil {
		return domain.Pipeline{}, err
	}
	// Strip pipeline-level fields a policy must not carry — a policy is a
	// fragment, not a standalone pipeline. Only stages + jobs survive.
	return domain.Pipeline{Stages: p.Stages, Jobs: p.Jobs}, nil
}

// ValidatePolicyNames enforces that every stage and job in a policy uses the
// reserved prefix, so merged jobs can never collide with or be shadowed by repo
// jobs (which RejectReservedNames forbids from using the prefix).
func ValidatePolicyNames(p domain.Pipeline) error {
	for _, s := range p.Stages {
		if !strings.HasPrefix(s, ReservedPrefix) {
			return fmt.Errorf("compliance: policy stage %q must start with %q", s, ReservedPrefix)
		}
	}
	for _, j := range p.Jobs {
		if !strings.HasPrefix(j.Name, ReservedPrefix) {
			return fmt.Errorf("compliance: policy job %q must start with %q", j.Name, ReservedPrefix)
		}
	}
	return nil
}

// RejectReservedNames is the repo-side guard: it fails if a project's own
// pipeline (parsed from repo YAML) uses the reserved compliance prefix for any
// stage or job. Called at apply time so a developer cannot pre-define a
// `_compliance_*` name to shadow or fake an enforced job.
func RejectReservedNames(raw domain.Pipeline) error {
	for _, s := range raw.Stages {
		if strings.HasPrefix(s, ReservedPrefix) {
			return fmt.Errorf("stage %q uses the reserved %q prefix (reserved for compliance policies)", s, ReservedPrefix)
		}
	}
	for _, j := range raw.Jobs {
		if strings.HasPrefix(j.Name, ReservedPrefix) {
			return fmt.Errorf("job %q uses the reserved %q prefix (reserved for compliance policies)", j.Name, ReservedPrefix)
		}
	}
	return nil
}
