package parser

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// validateNoCycles detects cycles in the `needs:` graph via DFS
// with three-color marking (classic algorithm). A cycle would
// otherwise deadlock the scheduler at runtime: every job in the
// cycle waits on another that's also waiting, all stay queued
// indefinitely, and nothing makes progress.
//
// Why this isn't covered by validateNeeds:
//   - forward-stage rejection only catches cycles that cross stages
//     in the wrong direction. Same-stage cycles (`a needs b`,
//     `b needs a` both in `build`) pass that check.
//   - self-reference rejection catches the 1-node cycle (`a needs a`).
//     But 2-cycle, 3-cycle, ... are not.
//
// Algorithm:
//   - white (unvisited) = 0
//   - gray (in current DFS stack) = 1
//   - black (fully explored) = 2
//     Hitting a gray node means we found a back-edge → cycle. The
//     stack at that moment is the cycle path; we slice from the
//     first occurrence of the revisited node to get a clean trace.
//
// Iteration order: caller may pass jobs in map-iteration order
// (non-deterministic). DFS visits jobs alphabetically so the
// error message is stable across runs — important when a
// pipeline fails apply in CI and the operator compares error
// strings across attempts.
func validateNoCycles(jobs []domain.Job) error {
	needs := make(map[string][]string, len(jobs))
	names := make([]string, 0, len(jobs))
	for _, j := range jobs {
		needs[j.Name] = j.Needs
		names = append(names, j.Name)
	}
	sort.Strings(names)

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(jobs))
	var stack []string

	var visit func(name string) error
	visit = func(name string) error {
		switch color[name] {
		case black:
			return nil
		case gray:
			// Back-edge → cycle. Slice the stack from where the
			// revisited name first appears so the trace is just
			// the cycle, not the whole DFS path leading to it.
			start := 0
			for i, n := range stack {
				if n == name {
					start = i
					break
				}
			}
			cycle := append([]string(nil), stack[start:]...)
			cycle = append(cycle, name)
			return fmt.Errorf("`needs:` cycle detected — jobs would deadlock at dispatch: %s", strings.Join(cycle, " → "))
		}
		color[name] = gray
		stack = append(stack, name)
		// Sort the dep list too so the error message is
		// deterministic when multiple cycles exist.
		deps := append([]string(nil), needs[name]...)
		sort.Strings(deps)
		for _, dep := range deps {
			if _, exists := needs[dep]; !exists {
				// Unknown name — already rejected by validateNeeds;
				// skip the recursion to avoid a nil-map traversal.
				// In the post-validateNeeds happy path this branch
				// never fires.
				continue
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return nil
	}

	for _, name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

// validateNeeds checks every job's `needs:` list against the set of
// jobs in the same pipeline. Rejects three classes of bug at apply
// time so the scheduler doesn't have to defend against them at
// dispatch:
//
//   - Unknown name: needs references a job that doesn't exist. The
//     scheduler's gate (server/internal/scheduler/needs.go) would
//     treat this as "terminal not-in-run" and silently SKIP the
//     downstream with `error="needs unmet: ghost: not in this run"`.
//     Stage/run cascade only counts `failed` (not `skipped`) toward
//     run failure (see results.sql GetStageProgress / GetRunProgress),
//     so a typo here would let the run finalize GREEN even though a
//     job was effectively unrunnable. Rejecting at apply means the
//     operator sees the typo before any run starts.
//
//   - Self-reference: `needs: [self]`. Same shape as "unknown" at
//     runtime (the job's own status drives the gate into a self-
//     wait), but a clearer error at apply.
//
//   - Forward-stage reference: a job in an earlier stage needs a
//     job in a later stage. The scheduler dispatches stages in
//     ordinal order; the later-stage job never starts until the
//     earlier stage closes, but the earlier-stage job can't close
//     because the gate is waiting on the later-stage job to
//     reach success. Hard deadlock — Kleber's pipeline would hang.
//     Same-stage and earlier-stage references are fine (the latter
//     is redundant given the stage gate but harmless).
func validateNeeds(jobs []domain.Job, stages []string) error {
	stageOrdinal := make(map[string]int, len(stages))
	for i, s := range stages {
		stageOrdinal[s] = i
	}
	byName := make(map[string]domain.Job, len(jobs))
	for _, j := range jobs {
		byName[j.Name] = j
	}
	for _, j := range jobs {
		myOrd, hasStage := stageOrdinal[j.Stage]
		for _, dep := range j.Needs {
			if dep == j.Name {
				return fmt.Errorf("job %q: `needs:` contains itself", j.Name)
			}
			target, exists := byName[dep]
			if !exists {
				return fmt.Errorf("job %q: `needs:` references unknown job %q (no job by that name in this pipeline)", j.Name, dep)
			}
			// If either job's stage isn't declared, the earlier
			// stage-check already rejected it; skip the ordinal
			// comparison to keep the error message focused on the
			// undeclared stage instead of compounding two errors.
			if !hasStage {
				continue
			}
			targetOrd, ok := stageOrdinal[target.Stage]
			if !ok {
				continue
			}
			if targetOrd > myOrd {
				return fmt.Errorf("job %q (stage %q, ordinal %d): `needs:` references %q in later stage %q (ordinal %d) — forward references would deadlock the dispatcher",
					j.Name, j.Stage, myOrd, dep, target.Stage, targetOrd)
			}
		}
	}
	return nil
}
