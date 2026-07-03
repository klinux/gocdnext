package domain

import "sort"

// Gate-governance graph (issue #97). Given gocdnext's SEQUENTIAL stages (stage
// N+1 waits for all of stage N) plus explicit job `needs:` edges, these resolve
// which approval gate governs a deploy to which environment. Pure functions over
// the pipeline definition — the supersede lane logic (Phase 1) and the gate-pass
// marker (Phase 2) both key off them.
//
// A deploy job D "waits for" a gate G iff G is in an earlier stage (stage
// sequence) or D transitively `needs:` G. Among the gates D waits for, its
// GOVERNING gates are the closest ones — a gate shadowed by a later waited-for
// gate does not govern D (the later gate is the real clearance). So in
// build→approve-staging→deploy-staging→approve-prod→deploy-prod, approve-staging
// governs only {staging} and approve-prod only {prod}.

func (p *Pipeline) stageIndex() map[string]int {
	idx := make(map[string]int, len(p.Stages))
	for i, s := range p.Stages {
		idx[s] = i
	}
	return idx
}

func (p *Pipeline) byName() map[string]Job {
	m := make(map[string]Job, len(p.Jobs))
	for _, j := range p.Jobs {
		m[j.Name] = j
	}
	return m
}

// transitivelyNeeds reports whether job j must run after `target` via the
// explicit `needs:` DAG (j needs … needs target).
func transitivelyNeeds(byName map[string]Job, j Job, target string) bool {
	seen := make(map[string]bool)
	var visit func(names []string) bool
	visit = func(names []string) bool {
		for _, n := range names {
			if n == target {
				return true
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			if child, ok := byName[n]; ok && visit(child.Needs) {
				return true
			}
		}
		return false
	}
	return visit(j.Needs)
}

// precedes reports whether job a must clear before job b can run: a is in an
// earlier stage (sequential stages are a barrier) OR b transitively `needs:` a.
// This is the "must-pass-before" partial order the governing-gate logic walks.
func precedes(sidx map[string]int, byName map[string]Job, a, b Job) bool {
	if sidx[a.Stage] < sidx[b.Stage] {
		return true
	}
	return transitivelyNeeds(byName, b, a.Name)
}

// governingGatesForJob returns the approval gates that govern deploy job d — the
// gates that must clear before d AND that no OTHER such gate shadows (a gate G is
// shadowed when a closer gate G2 also-clearing-before-d sits between them:
// G ≺ G2 ≺ d). Handles same-stage gate chains via `needs:` (approve-security →
// approve-prod → deploy-prod ⇒ only approve-prod governs), not just stage order.
func (p *Pipeline) governingGatesForJob(d Job) []string {
	sidx := p.stageIndex()
	byName := p.byName()

	var waits []Job // gates that must clear before d
	for _, g := range p.Jobs {
		if g.Approval != nil && precedes(sidx, byName, g, d) {
			waits = append(waits, g)
		}
	}
	var out []string
	for _, g := range waits {
		shadowed := false
		for _, g2 := range waits {
			if g2.Name != g.Name && precedes(sidx, byName, g, g2) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			out = append(out, g.Name)
		}
	}
	return out
}

// GovernedEnvs returns the sorted, deduped CONCRETE deploy environment names
// that approval gate `gateName` governs. Empty when the gate governs no deploy
// job (a pure-approval gate — writes no run_gate_pass marker, has whole-run
// scope only for the Phase 1 pile-clear).
func (p *Pipeline) GovernedEnvs(gateName string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, d := range p.Jobs {
		if d.Deploy == nil || d.Deploy.Environment == "" {
			continue
		}
		for _, g := range p.governingGatesForJob(d) {
			if g != gateName {
				continue
			}
			if _, dup := seen[d.Deploy.Environment]; !dup {
				seen[d.Deploy.Environment] = struct{}{}
				out = append(out, d.Deploy.Environment)
			}
		}
	}
	sort.Strings(out)
	return out
}

// GoverningGates returns the sorted approval-gate job names that govern a deploy
// to `env`. A run is cleared to deploy env once ALL of these have passed (the
// Phase 2 marker is written only then).
func (p *Pipeline) GoverningGates(env string) []string {
	set := make(map[string]struct{})
	for _, d := range p.Jobs {
		if d.Deploy == nil || d.Deploy.Environment != env {
			continue
		}
		for _, g := range p.governingGatesForJob(d) {
			set[g] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}
