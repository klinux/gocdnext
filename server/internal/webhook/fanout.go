package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// fanOutInput collects everything fanOutMaterials needs to spawn a
// run per matching material. The fields that vary across providers
// (github vs gitlab vs bitbucket) live on the call site; everything
// below is provider-agnostic.
type fanOutInput struct {
	// Materials matched by FingerprintFor(repoURL, branch). One run
	// is created per material — i.e. per pipeline that shares the
	// same (repo, branch).
	Materials []store.Material

	// Revision is the commit SHA / tag the push points at. Becomes
	// modification.revision AND CreateRunFromModification.Revision.
	Revision string
	// Branch lands on modification.branch.
	Branch string
	// Author, Message, CommittedAt populate the modification row so
	// the UI can show "who/what/when" without a follow-up git call.
	Author      string
	Message     string
	CommittedAt time.Time
	// Payload is the raw provider body — stored on modification so
	// future reprocessing has full context.
	Payload json.RawMessage

	// Provider, Delivery, TriggeredBy ride on the created run row.
	Provider    string
	Delivery    string
	TriggeredBy string

	// Cause + CauseDetail let the pull_request path stamp the run
	// with its provenance (PR number, head ref, action). Push
	// handlers leave these empty and let the store default to
	// "push" — keeps the shape uniform without forcing every
	// caller to pass identical zeroes.
	Cause       string
	CauseDetail json.RawMessage
}

// fanOutOutcome reports what happened for ONE matched material. The
// caller aggregates them into the 202 body so the operator sees
// every (pipeline, run, status) tuple instead of just the first one.
type fanOutOutcome struct {
	PipelineID     uuid.UUID
	MaterialID     uuid.UUID
	ModificationID int64
	ModCreated     bool // false when InsertModification deduped (same revision retried)
	RunID          uuid.UUID
	RunCounter     int64
	// Err is non-nil for partial failure: the modification may have
	// landed but the run creation errored, OR the modification insert
	// itself errored. The caller logs and skips this entry but does
	// NOT fail the whole delivery — other pipelines' runs are still
	// valuable signal for the operator.
	Err error
}

// fanOutMaterials walks each material in the input set, inserts a
// modification (idempotent on material_id+revision+branch), and on
// CREATE-NOT-DUPLICATE creates a run for the owning pipeline. Errors
// per material are returned in the per-outcome Err field so one bad
// pipeline doesn't take down all the others on the same delivery.
//
// Concurrency: serial today. The dispatch path is already async
// (LISTEN/NOTIFY wakes the scheduler), so the latency of N
// CreateRunFromModification calls is fine for N < ~20 pipelines per
// repo+branch. Parallelising would buy us little and trade for
// harder per-error attribution.
func fanOutMaterials(ctx context.Context, log *slog.Logger, s *store.Store, in fanOutInput) []fanOutOutcome {
	out := make([]fanOutOutcome, 0, len(in.Materials))
	for _, m := range in.Materials {
		oc := fanOutOutcome{PipelineID: m.PipelineID, MaterialID: m.ID}
		mod, err := s.InsertModification(ctx, store.Modification{
			MaterialID:  m.ID,
			Revision:    in.Revision,
			Branch:      in.Branch,
			Author:      in.Author,
			Message:     in.Message,
			Payload:     in.Payload,
			CommittedAt: in.CommittedAt,
		})
		if err != nil {
			oc.Err = fmt.Errorf("insert modification: %w", err)
			log.Warn("webhook fan-out: modification insert failed",
				"pipeline_id", m.PipelineID, "material_id", m.ID,
				"delivery", in.Delivery, "err", err)
			out = append(out, oc)
			continue
		}
		oc.ModificationID = mod.ID
		oc.ModCreated = mod.Created

		// Only create a run when the modification is brand new; on
		// provider retries the dedup keeps us from spawning a second
		// run for the same SHA. Each pipeline's run is independent —
		// a stale dedup on pipeline A doesn't block pipeline B.
		if !mod.Created {
			out = append(out, oc)
			continue
		}

		run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
			PipelineID:     m.PipelineID,
			MaterialID:     m.ID,
			ModificationID: mod.ID,
			Revision:       in.Revision,
			Branch:         in.Branch,
			Provider:       in.Provider,
			Delivery:       in.Delivery,
			TriggeredBy:    in.TriggeredBy,
			Cause:          in.Cause,
			CauseDetail:    in.CauseDetail,
		})
		if err != nil {
			oc.Err = fmt.Errorf("create run: %w", err)
			log.Warn("webhook fan-out: run creation failed",
				"pipeline_id", m.PipelineID, "material_id", m.ID,
				"modification_id", mod.ID, "delivery", in.Delivery, "err", err)
			out = append(out, oc)
			continue
		}
		oc.RunID = run.RunID
		oc.RunCounter = run.Counter
		out = append(out, oc)
	}
	return out
}

// runsPayload turns the fan-out outcomes into the wire shape the 202
// body advertises: an array of (pipeline_id, run_id, counter) for
// every successfully-created run. Failures are intentionally NOT
// listed here — they're already in the server log; the response body
// summarises what the operator can act on (open the run).
func runsPayload(outcomes []fanOutOutcome) []map[string]any {
	runs := make([]map[string]any, 0, len(outcomes))
	for _, oc := range outcomes {
		if oc.Err != nil || oc.RunID == uuid.Nil {
			continue
		}
		runs = append(runs, map[string]any{
			"pipeline_id":  oc.PipelineID.String(),
			"material_id":  oc.MaterialID.String(),
			"run_id":       oc.RunID.String(),
			"run_counter":  oc.RunCounter,
		})
	}
	return runs
}

// firstCreatedRunMaterialID returns the material id of the first
// outcome that actually produced a run, or uuid.Nil when nothing
// was created (every modification deduped). Used to populate
// rec.materialID for the audit row — pre-fan-out the handler had
// exactly one material to record, post-fan-out there are N, but
// the audit row still expects ONE material_id reference.
func firstCreatedRunMaterialID(outcomes []fanOutOutcome) uuid.UUID {
	for _, oc := range outcomes {
		if oc.Err == nil && oc.RunID != uuid.Nil {
			return oc.MaterialID
		}
	}
	return uuid.Nil
}
