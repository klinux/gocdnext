package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// manualUpstreamContext is the cause_detail + revisions + supersede ref a manual
// trigger of an upstream-driven pipeline inherits from the latest successful
// upstream run — the pull-side mirror of what the fanout stamps on the push side.
type manualUpstreamContext struct {
	causeDetail json.RawMessage
	revisions   json.RawMessage
	ref         string
}

// resolveManualUpstreamContext resolves the pipeline's `upstream` material to
// the latest successful run of the upstream pipeline, returning the cause_detail
// (upstream_run_id/counter/pipeline/stage, merged onto any callerDetail), the
// revisions, and the supersede ref the fanout would stamp.
//
// resolved == true only when the pipeline declares EXACTLY ONE upstream material
// AND that upstream has a successful run. resolved == false covers "no upstream
// material", "multiple upstream materials" (ambiguous — counters aren't
// comparable across pipelines, so we don't guess) and "has one but no green run
// yet"; the caller falls back to its existing git-modification / bare-skeleton
// path unchanged, so a standalone downstream stays hand-kickable.
//
// Resolving here (not just at fanout time) is what lets a hand-kicked deploy
// rebuild the SAME 1.<counter>.<sha> the upstream produced, on the SAME lane:
// counter, commit AND ref are pulled together from the one build run, never
// mixing the build's counter with the deploy repo's live HEAD.
func (s *Store) resolveManualUpstreamContext(ctx context.Context, pipelineID uuid.UUID, callerDetail []byte) (manualUpstreamContext, bool, error) {
	// Auto-resolve only when there is exactly one upstream material: run counters
	// are per-pipeline, so "newest across two upstreams" is meaningless. Anything
	// else falls back to the plain manual path rather than deploy the wrong build.
	n, err := s.q.CountUpstreamMaterials(ctx, pgUUID(pipelineID))
	if err != nil {
		return manualUpstreamContext{}, false, fmt.Errorf("store: manual upstream: material count: %w", err)
	}
	if n != 1 {
		return manualUpstreamContext{}, false, nil
	}

	row, err := s.q.LatestUpstreamRunForManualTrigger(ctx, pgUUID(pipelineID))
	if errors.Is(err, pgx.ErrNoRows) {
		return manualUpstreamContext{}, false, nil
	}
	if err != nil {
		return manualUpstreamContext{}, false, fmt.Errorf("store: manual upstream: latest run: %w", err)
	}

	// Merge the upstream keys onto whatever the caller stamped (project cron
	// passes schedule_id / schedule_name / expression). Upstream keys are
	// authoritative on collision. A malformed callerDetail — OR the literal JSON
	// `null`, which unmarshals a map to nil — degrades to an empty map, so the
	// assignments below never write into a nil map (which would panic).
	detail := map[string]any{}
	if len(callerDetail) > 0 {
		if err := json.Unmarshal(callerDetail, &detail); err != nil || detail == nil {
			detail = map[string]any{}
		}
	}
	upstreamRunID := fromPgUUID(row.UpstreamRunID)
	detail["upstream_run_id"] = upstreamRunID.String()
	detail["upstream_run_counter"] = row.UpstreamRunCounter
	detail["upstream_pipeline"] = row.UpstreamPipeline
	detail["upstream_stage"] = row.UpstreamStage
	// Marks a run whose upstream was resolved at trigger time (pull-side), as
	// opposed to a fanout-created run (cause="upstream", push-side). Deliberately
	// cause-agnostic — the trigger may be a manual kick OR a schedule — so the UI
	// labels the banner by the run's cause, not by this marker.
	detail["resolved_upstream"] = true
	causeDetail, err := json.Marshal(detail)
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

	return manualUpstreamContext{causeDetail: causeDetail, revisions: revisions, ref: row.UpstreamRef}, true, nil
}
