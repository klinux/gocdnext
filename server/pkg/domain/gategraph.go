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

// governingGatesForJob returns the approval-gate job names that govern deploy
// job d (closest gates, shadowing removed). Unexported: callers use
// GovernedEnvs / GoverningGates.
func (p *Pipeline) governingGatesForJob(d Job) []string {
	sidx := p.stageIndex()
	dStage := sidx[d.Stage]

	// waits: gate name -> its stage index (gates D must clear before running).
	waits := make(map[string]int)
	for _, g := range p.Jobs {
		if g.Approval == nil {
			continue
		}
		if gs := sidx[g.Stage]; gs < dStage { // earlier stage → stage-sequence dep
			waits[g.Name] = gs
		}
	}
	// transitive needs add same-stage gates + explicit cross edges.
	for _, gn := range p.transitiveNeedGates(d) {
		waits[gn] = sidx[p.gateStage(gn)]
	}

	var out []string
	for gn, gs := range waits {
		shadowed := false
		for other, os := range waits {
			if other != gn && gs < os && os < dStage {
				shadowed = true
				break
			}
		}
		if !shadowed {
			out = append(out, gn)
		}
	}
	return out
}

func (p *Pipeline) gateStage(name string) string {
	for _, j := range p.Jobs {
		if j.Name == name {
			return j.Stage
		}
	}
	return ""
}

// transitiveNeedGates walks the `needs:` DAG from d and returns every approval
// gate reachable. Shadowing (closest-gate selection) is applied by the caller.
func (p *Pipeline) transitiveNeedGates(d Job) []string {
	byName := make(map[string]Job, len(p.Jobs))
	for _, j := range p.Jobs {
		byName[j.Name] = j
	}
	var gates []string
	seen := make(map[string]bool)
	var visit func(names []string)
	visit = func(names []string) {
		for _, n := range names {
			if seen[n] {
				continue
			}
			seen[n] = true
			j, ok := byName[n]
			if !ok {
				continue
			}
			if j.Approval != nil {
				gates = append(gates, n)
			}
			visit(j.Needs)
		}
	}
	visit(d.Needs)
	return gates
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
