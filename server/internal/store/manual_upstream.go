package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// manualUpstreamContext is the cause_detail + revisions a manual trigger of an
// upstream-driven pipeline inherits from the latest successful upstream run —
// the pull-side mirror of what the fanout stamps on the push side.
type manualUpstreamContext struct {
	causeDetail json.RawMessage
	revisions   json.RawMessage
}

// resolveManualUpstreamContext resolves the pipeline's `upstream` material to
// the latest successful run of the upstream pipeline, returning the cause_detail
// (upstream_run_id/counter/pipeline/stage) + revisions the fanout would stamp.
//
// resolved == true only when a successful upstream run exists; the caller then
// seeds the manual run from it. resolved == false covers BOTH "no upstream
// material" and "has one but its upstream has no green run yet" — the caller
// falls back to its existing git-modification / bare-skeleton path unchanged, so
// a standalone downstream stays hand-kickable before its upstream ever lands.
//
// Resolving here (not just at fanout time) is what lets a hand-kicked deploy
// rebuild the SAME 1.<counter>.<sha> the upstream produced: the counter AND the
// commit are pulled together from the one build run, never mixing the build's
// counter with the deploy repo's live HEAD (which would template an image that
// was never built).
func (s *Store) resolveManualUpstreamContext(ctx context.Context, pipelineID uuid.UUID) (manualUpstreamContext, bool, error) {
	row, err := s.q.LatestUpstreamRunForManualTrigger(ctx, pgUUID(pipelineID))
	if errors.Is(err, pgx.ErrNoRows) {
		return manualUpstreamContext{}, false, nil
	}
	if err != nil {
		return manualUpstreamContext{}, false, fmt.Errorf("store: manual upstream: latest run: %w", err)
	}

	upstreamRunID := fromPgUUID(row.UpstreamRunID)
	causeDetail, err := json.Marshal(map[string]any{
		"upstream_run_id":      upstreamRunID.String(),
		"upstream_run_counter": row.UpstreamRunCounter,
		"upstream_pipeline":    row.UpstreamPipeline,
		"upstream_stage":       row.UpstreamStage,
		// Marks a hand-kicked deploy that resolved to the latest build, as
		// opposed to a fanout-created run (cause="upstream"). The run stays
		// honestly cause="manual" while still surfacing CI_UPSTREAM_*.
		"manual_upstream": true,
	})
	if err != nil {
		return manualUpstreamContext{}, false, fmt.Errorf("store: manual upstream: marshal detail: %w", err)
	}

	// Base the run's revisions on the upstream run's — the same git commit the
	// build produced, so CI_COMMIT_SHA matches the counter — then stamp the
	// downstream `upstream` material (branchless "revision" = the upstream run
	// UUID) so primaryRevision keeps preferring the git checkout, exactly as the
	// fanout path does.
	revs := map[string]any{}
	if len(row.UpstreamRevisions) > 0 {
		if err := json.Unmarshal(row.UpstreamRevisions, &revs); err != nil {
			return manualUpstreamContext{}, false, fmt.Errorf("store: manual upstream: decode upstream revisions: %w", err)
		}
	}
	revs[fromPgUUID(row.DownstreamMaterialID).String()] = map[string]string{
		"revision": upstreamRunID.String(),
		"branch":   "",
	}
	revisions, err := json.Marshal(revs)
	if err != nil {
		return manualUpstreamContext{}, false, fmt.Errorf("store: manual upstream: marshal revisions: %w", err)
	}

	return manualUpstreamContext{causeDetail: causeDetail, revisions: revisions}, true, nil
}
