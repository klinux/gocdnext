package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
	"github.com/google/uuid"
)

// failJobNeedsUnmet marks a still-queued job as `failed` when one
// of its declared upstreams reached a non-success terminal state
// (failed / canceled / skipped / missing). The human-readable
// reason lands on the job's `error` column so the operator can
// grep the chain back to the root cause. The
// cascadeAfterJobCompletion baked into FailJobWithReason closes
// the stage + run terminal logic — without that cascade, the
// stage would hang on the queued job forever.
//
// Why `failed` (not `skipped`): GetStageProgress and
// GetRunUserStageOutcome only count `status='failed'` toward the
// run-failed aggregate. A `skipped` downstream from needs-cascade
// would leak through as run = success despite a job that
// EXPECTED to run never running — confusing operator, fanout,
// and `on: success` notifications. The `error` column carries
// the chain so UI / API can distinguish a true agent-side
// failure from a needs-cascade failure. Notification trigger
// skips (SkipNotificationJob) stay `skipped` because there the
// semantic is "by design, never going to run" — different from
// needs-cascade where the operator wrote `needs: [X]` expecting
// X to succeed.
//
// Defense-in-depth: even though d78c8f5's parser validation
// rejects unknown / forward / self `needs:` at apply time, a
// snapshot drift (older parser, schema change, manual DB poke)
// could still produce a runtime needs-unmet. This path catches
// that case so the run can never finalize as silent-success.
func (s *Scheduler) failJobNeedsUnmet(ctx context.Context, job store.DispatchableJob, detail string) {
	msg := "needs unmet: " + detail
	if _, _, err := s.store.FailJobWithReason(ctx, job.ID, msg); err != nil {
		s.log.Warn("scheduler: fail job needs-unmet",
			"job_id", job.ID, "job_name", job.Name, "err", err)
		return
	}
	s.log.Warn("scheduler: job failed — needs unmet (upstream non-success)",
		"run_id", job.RunID, "job_id", job.ID, "job_name", job.Name, "reason", msg)
}

// buildNeedsOutputs assembles the NeedsOutputs table for a downstream
// job's `${{ needs.X.outputs.Y }}` substitution (issue #10). Scoped
// to job.Needs so the query is cheap; runs AFTER the gate so all
// upstream rows are terminal-success.
//
// Empty job.Needs short-circuits — most jobs don't declare needs and
// shouldn't pay for a DB round-trip.
func (s *Scheduler) buildNeedsOutputs(ctx context.Context, runID uuid.UUID, job store.DispatchableJob) (NeedsOutputs, MatrixNeedsOutputs, error) {
	if len(job.Needs) == 0 {
		return nil, nil, nil
	}
	rows, err := s.store.ListJobOutputsForRun(ctx, runID, job.Needs)
	if err != nil {
		return nil, nil, fmt.Errorf("list job outputs: %w", err)
	}
	return groupNeedsOutputs(rows)
}

// groupNeedsOutputs is the pure grouping helper extracted from
// buildNeedsOutputs so the matrix routing can be tested without
// a DB round-trip. Public-package-local; not exported.
//
// Matrix routing (issue #21) keys on the row's `matrix_key`,
// not on row count:
//   - matrix_key="" (non-matrix upstream) → NeedsOutputs[name].
//     The downstream substitution reaches it via bare
//     `${{ needs.X.outputs.Y }}`. The parser rejects empty
//     matrix entries / zero-dim matrices at apply time, so a
//     job declaring `parallel.matrix` cannot produce a row
//     here.
//   - matrix_key!="" (any matrix expansion, including N==1) →
//     MatrixNeedsOutputs[name][canon]. `canon` is the row's
//     matrix_key normalised to lex-sorted k=v,k=v form (the
//     substitution layer canonicalizes its selector body the
//     same way before lookup). Downstream reaches each via
//     `${{ needs.X.matrix[k=v,...].outputs.Y }}`.
//
// Non-success upstream rows are dropped silently — the
// needsSatisfied gate already blocks dispatch when an upstream
// isn't terminal-success, so this is defence in depth. A
// downstream ref against a dropped row falls through to
// substituteNeedsRefs' loud-error path.
func groupNeedsOutputs(rows []store.JobOutputs) (NeedsOutputs, MatrixNeedsOutputs, error) {
	grouped := make(map[string][]store.JobOutputs)
	for _, r := range rows {
		grouped[r.Name] = append(grouped[r.Name], r)
	}
	plain := NeedsOutputs{}
	matrix := MatrixNeedsOutputs{}
	for name, group := range grouped {
		// Filter to successful rows up-front so the multi-row
		// branch's classification only counts real expansions.
		successful := group[:0]
		for _, g := range group {
			if g.Status == string(domain.StatusSuccess) {
				successful = append(successful, g)
			}
		}
		if len(successful) == 0 {
			continue
		}
		if len(successful) == 1 && successful[0].MatrixKey == "" {
			// Non-matrix upstream — the bare-ref path's home.
			plain[name] = successful[0].Outputs
			continue
		}
		// Matrix expansion (one or more rows with a non-empty
		// matrix_key). Even N=1 lands here — the operator
		// declared `strategy.matrix`, the downstream must use
		// the selector to be explicit.
		rowMap := make(map[string]map[string]string, len(successful))
		for _, g := range successful {
			canon, err := canonicalizeMatrixKey(g.MatrixKey)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"upstream job %q has malformed matrix_key %q: %w",
					name, g.MatrixKey, err)
			}
			// Duplicate canonical key = the matrix expanded to
			// two rows that collide on the lex-sorted k=v form.
			// The parser already rejects within-dimension
			// duplicates at apply time (validateMatrixDimensions),
			// so the only way to reach this branch is data
			// corruption or a future regression — refuse loud
			// rather than overwrite the prior row silently and
			// leak operator-invisible non-determinism into the
			// downstream substitution.
			if _, dup := rowMap[canon]; dup {
				return nil, nil, fmt.Errorf(
					"upstream job %q has duplicate canonical matrix_key %q (rows collide on the lex-sorted k=v form); "+
						"this is supposed to be impossible after parser validation — investigate as a data-shape regression",
					name, canon)
			}
			rowMap[canon] = g.Outputs
		}
		matrix[name] = rowMap
	}
	return plain, matrix, nil
}

// canonicalizeMatrixKey takes a stored matrix_key string from
// store.JobOutputs (the agent reports rows like "shard=apac"
// or "region=br,shard=apac" — order is whatever the parser
// emitted) and returns the lex-sorted form. The substitution
// layer canonicalizes its selector body the same way before
// the lookup so a YAML author who writes
// `matrix[shard=apac,region=br]` resolves to the same canonical
// key as the stored `region=br,shard=apac`.
func canonicalizeMatrixKey(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	parts := strings.Split(stored, ",")
	keys := make([]string, 0, len(parts))
	values := make(map[string]string, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		eq := strings.IndexByte(p, '=')
		if eq <= 0 || eq == len(p)-1 {
			return "", fmt.Errorf("part %q is not k=v", p)
		}
		k, v := p[:eq], p[eq+1:]
		if _, dup := values[k]; dup {
			return "", fmt.Errorf("repeated dim %q", k)
		}
		values[k] = v
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(values[k])
	}
	return b.String(), nil
}
