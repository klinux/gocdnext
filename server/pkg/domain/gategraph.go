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

// ReadyGateEnvsAtStart returns the concrete deploy envs governed by the approval
// gates that are READY the instant a run is created — gates in the first stage with
// no unmet needs (nothing precedes them). Used by the #97 creation supersede fire:
// a new run pending at such a gate clears older lane siblings pending for the same
// env. `ready` is false when the run has NO gate ready at creation (fire nothing);
// `ready` true with an EMPTY env set means the ready gate governs no deploy — a
// whole-run pile-clear scope, distinct from "no ready gate". Inline gates (later
// stages) become ready mid-run and fire from the completion cascade instead.
func (p *Pipeline) ReadyGateEnvsAtStart() (envs []string, ready bool) {
	if len(p.Stages) == 0 {
		return nil, false
	}
	first := p.Stages[0]
	seen := make(map[string]struct{})
	for _, j := range p.Jobs {
		// A gate with any needs can't be ready at creation — nothing has run yet,
		// so no need is satisfied. Only a needs-free first-stage gate is reachable.
		if j.Approval == nil || j.Stage != first || len(j.Needs) > 0 {
			continue
		}
		ready = true
		for _, e := range p.GovernedEnvs(j.Name) {
			if _, dup := seen[e]; !dup {
				seen[e] = struct{}{}
				envs = append(envs, e)
			}
		}
	}
	sort.Strings(envs)
	return envs, ready
}

// ReadyGateEnvsAfterStage returns the concrete deploy envs governed by the approval
// gates that become READY the moment the stage at completedOrdinal finishes — gates
// in the NEXT stage whose needs are all satisfied by then (needs pointing only at
// jobs in already-finished stages, ordinal <= completedOrdinal). Used by the #97
// cascade supersede fire: stages run sequentially, so the frontier advances one
// stage per completion; a gate deeper down fires when ITS predecessor completes.
// `ready` is false when the next stage has no reachable gate (or there is no next
// stage). Same empty-envs-but-ready semantics as ReadyGateEnvsAtStart (pure-approval
// gate = whole-run scope).
func (p *Pipeline) ReadyGateEnvsAfterStage(completedOrdinal int) (envs []string, ready bool) {
	next := completedOrdinal + 1
	if next <= 0 || next >= len(p.Stages) {
		return nil, false
	}
	nextStage := p.Stages[next]
	sidx := p.stageIndex()
	byName := p.byName()
	seen := make(map[string]struct{})
	for _, j := range p.Jobs {
		if j.Approval == nil || j.Stage != nextStage || !gateNeedsSatisfied(sidx, byName, j, completedOrdinal) {
			continue
		}
		ready = true
		for _, e := range p.GovernedEnvs(j.Name) {
			if _, dup := seen[e]; !dup {
				seen[e] = struct{}{}
				envs = append(envs, e)
			}
		}
	}
	sort.Strings(envs)
	return envs, ready
}

// gateNeedsSatisfied reports whether every direct need of gate g points at a job in
// a stage that has already finished (ordinal <= completedOrdinal). A need on a
// same-stage job (the gate's own stage) isn't done yet, so the gate isn't ready
// this tick; an unknown need is treated as unsatisfied (play safe, don't fire).
func gateNeedsSatisfied(sidx map[string]int, byName map[string]Job, g Job, completedOrdinal int) bool {
	for _, n := range g.Needs {
		dep, ok := byName[n]
		if !ok || sidx[dep.Stage] > completedOrdinal {
			return false
		}
	}
	return true
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
