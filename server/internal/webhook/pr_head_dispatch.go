package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

var (
	// ErrPRHeadBinding fails the REPO pipelines closed because the PR's clone URL
	// maps to more than one OPTED-IN project — an ambiguous binding we refuse to
	// guess between. Operator misconfiguration, not retryable → 422.
	ErrPRHeadBinding = errors.New("pr-head: ambiguous scm binding")
	// ErrPRHeadUnavailable fails the REPO pipelines closed because a transient
	// store error (identity or binding lookup) prevented resolving the head plan.
	// Retryable → 503. We do NOT silently run the base definition for a head-enabled
	// repo when we can't even determine the partition.
	ErrPRHeadUnavailable = errors.New("pr-head: temporarily unavailable")
)

// defaultPRHeadResolveTimeout bounds the TOTAL head-config resolution (a folder
// can need many sequential requests, each with its own HTTP timeout). Without it
// a slow contributor branch could delay this delivery for the whole request
// budget. Derived from the request context, so a client disconnect still cancels
// promptly. Overridable per-Handler (tests set it low); zero falls back here.
const defaultPRHeadResolveTimeout = 30 * time.Second

// applyPRHeadConfig runs the PR-head-config path for a matched PR/MR and returns
// (outcomes, resolveErr):
//   - outcomes: EVERY run this delivery created — the base/system_managed fan-out
//     PLUS any head runs — so the caller folds them all into the response;
//   - resolveErr: a TYPED, fail-closed error that blocked the REPO pipelines
//     (ambiguous binding, a transient lookup failure, or a fetch/parse/validate
//     error). The base/system_managed runs in outcomes still ran; the caller maps
//     resolveErr to 503/422 EVEN SO — the repo pipeline is genuinely missing from
//     this delivery, and for a transient error the provider must retry.
//
// It is a strict base-only path (ZERO fetch) for anything but a GitHub same-repo
// PR on an opted-in project. Invariants:
//   - system_managed ALWAYS runs base, independent of the head;
//   - a repo material runs from the head only with exactly ONE opted-in binding
//     for its project; 0 opted-in → base flow; >1 opted-in → fail closed (422);
//   - a transient identity/binding lookup error fails closed (503), never a silent
//     base run for a head-enabled repo;
//   - BASE-FIRST: the base/system_managed pipelines are fanned out BEFORE the
//     (slow) head resolution, so a slow contributor branch never delays the
//     mandatory pipelines and a client disconnect mid-resolution can't cancel
//     their creation.
func (h *Handler) applyPRHeadConfig(
	ctx context.Context, ev pullRequestEvent, materials []store.Material,
	causeDetail json.RawMessage, delivery string, body []byte,
) (outcomes []fanOutOutcome, resolveErr error) {
	if ev.Provider != "github" || !ev.SameRepo {
		return h.fanOutPRBase(ctx, ev, materials, causeDetail, delivery, body), nil
	}

	ids := make([]uuid.UUID, len(materials))
	for i, m := range materials {
		ids[i] = m.ID
	}
	identities, err := h.store.ListMaterialPipelineIdentities(ctx, ids)
	if err != nil {
		// Can't partition ⇒ can't tell a mandatory pipeline from a head-driven one.
		// Fail closed WITHOUT running anything: silently running the base definition
		// would violate the head-config contract, and a store blip is retryable.
		h.log.Error("pr-head: material identity lookup failed — failing closed",
			"repo", ev.RepoLabel, "delivery", delivery, "err", err)
		return nil, fmt.Errorf("%w: identity lookup", ErrPRHeadUnavailable)
	}
	byMaterial := make(map[uuid.UUID]store.MaterialPipelineIdentity, len(identities))
	for _, id := range identities {
		byMaterial[id.MaterialID] = id
	}

	// Partition: repo (non-system-managed) candidates vs base (system_managed, or a
	// material with no identity — never head-driven).
	var repo, base []store.Material
	for _, m := range materials {
		if id, ok := byMaterial[m.ID]; ok && !id.SystemManaged {
			repo = append(repo, m)
		} else {
			base = append(base, m)
		}
	}
	if len(repo) == 0 {
		// Only system_managed (or identity-less) work → base flow, ZERO fetch.
		return h.fanOutPRBase(ctx, ev, materials, causeDetail, delivery, body), nil
	}

	bindings, err := h.store.FindPRHeadBindingsByURL(ctx, ev.CloneURL)
	if err != nil {
		// system_managed still runs; the repo plan is blocked by a transient store
		// error → 503, base runs included in the response.
		h.log.Error("pr-head: scm binding lookup failed — repo pipelines blocked",
			"repo", ev.RepoLabel, "delivery", delivery, "err", err)
		return h.fanOutPRBase(ctx, ev, base, causeDetail, delivery, body),
			fmt.Errorf("%w: binding lookup", ErrPRHeadUnavailable)
	}
	// Only an OPTED-IN (trusted) binding carries head config. The clone URL is NOT
	// unique, so filter to trusted before deciding: 0 trusted → nobody opted in, run
	// base for everyone (incl. repo, unchanged); >1 trusted → two opted-in projects
	// on the same URL, we refuse to guess which head config wins → fail closed. This
	// keeps a project that never opted in from being blocked just because it shares
	// a clone URL with an opted-in one.
	var trusted []store.PRHeadBinding
	for _, b := range bindings {
		if b.Trust {
			trusted = append(trusted, b)
		}
	}
	switch len(trusted) {
	case 0:
		return h.fanOutPRBase(ctx, ev, materials, causeDetail, delivery, body), nil
	case 1:
		// exactly one opted-in binding — proceed with trusted[0]
	default:
		h.log.Warn("pr-head: ambiguous scm binding (>1 opted-in project) — repo pipelines blocked",
			"repo", ev.RepoLabel, "delivery", delivery, "opted_in", len(trusted))
		return h.fanOutPRBase(ctx, ev, base, causeDetail, delivery, body), ErrPRHeadBinding
	}
	b := trusted[0]

	// Base-authorized set: a repo material in THIS project drives from the head; a
	// repo material of ANOTHER project sharing the URL falls back to base.
	var authorized []authorizedPipeline
	for _, m := range repo {
		id := byMaterial[m.ID]
		if id.ProjectID != b.Source.ProjectID {
			base = append(base, m)
			continue
		}
		authorized = append(authorized, authorizedPipeline{
			Name: id.PipelineName, PipelineID: id.PipelineID, MaterialID: id.MaterialID,
		})
	}

	// BASE-FIRST: dispatch the mandatory/base pipelines before the slow resolution.
	baseOutcomes := h.fanOutPRBase(ctx, ev, base, causeDetail, delivery, body)
	if len(authorized) == 0 {
		// Every repo material belonged to another project → nothing to resolve.
		return baseOutcomes, nil
	}

	timeout := h.prHeadResolveTimeout
	if timeout <= 0 {
		timeout = defaultPRHeadResolveTimeout
	}
	resolveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	plan, err := resolvePRHeadPlan(resolveCtx, h.fetcher, h.pluginCatalog, b.Source, b.ConfigPath, ev.HeadSHA, authorized)
	elapsed := time.Since(started)
	// Duration + result of the head resolution (observability the plan calls for):
	// a Prometheus histogram keyed to a bounded outcome label, plus a structured log.
	metrics.PRHeadResolution.WithLabelValues(prHeadResolveOutcome(err)).Observe(elapsed.Seconds())
	h.log.Info("pr-head: config resolution",
		"repo", ev.RepoLabel, "delivery", delivery, "pipelines", len(authorized),
		"duration_ms", elapsed.Milliseconds(), "ok", err == nil)
	if err != nil {
		// Resolution error blocks the repo pipelines of this plan (no partial repo
		// runs); base already ran. The caller maps err to 503/422.
		h.log.Warn("pr-head: config resolution failed — repo pipelines blocked",
			"repo", ev.RepoLabel, "delivery", delivery, "err", err)
		return baseOutcomes, err
	}
	// One outcome per plan entry — per-material, preserving per-pipeline failure
	// semantics (a store admission error on one pipeline doesn't sink the others).
	headOutcomes := make([]fanOutOutcome, 0, len(plan))
	for _, e := range plan {
		headOutcomes = append(headOutcomes, h.createPRHeadRun(ctx, ev, b, e, causeDetail, delivery, body))
	}
	return append(baseOutcomes, headOutcomes...), nil
}

// fanOutPRBase creates one run per material on the BASE flow (the project's
// registered default-branch definition), stamped with the PR/MR provenance. It is
// the mandatory path for system_managed pipelines and the fallback for every
// non-head material.
func (h *Handler) fanOutPRBase(
	ctx context.Context, ev pullRequestEvent, base []store.Material,
	causeDetail json.RawMessage, delivery string, body []byte,
) []fanOutOutcome {
	if len(base) == 0 {
		return nil
	}
	return fanOutMaterials(ctx, h.log, h.store, fanOutInput{
		Materials:   base,
		Revision:    ev.HeadSHA,
		Branch:      ev.HeadRef,
		Author:      ev.Author,
		Message:     ev.Title,
		Payload:     json.RawMessage(body),
		CommittedAt: ev.At,
		Provider:    ev.Provider,
		Delivery:    delivery,
		TriggeredBy: "system:webhook",
		Cause:       string(domain.CausePullRequest),
		CauseDetail: causeDetail,
	})
}

// prHeadResolveOutcome maps a resolver error to the bounded metric label set
// (never the error message).
func prHeadResolveOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrPRHeadFetch):
		return "fetch_error"
	default:
		return "invalid"
	}
}

// prHeadErrorResponse maps a fail-closed head error to an HTTP status + a STABLE,
// sanitized message safe to persist and return (never the raw resolver detail,
// which stays in the server log):
//   - fetch / transient lookup → 503 (retryable);
//   - ambiguous binding / invalid config → 422 (not retryable).
func prHeadErrorResponse(err error) (int, string) {
	switch {
	case errors.Is(err, ErrPRHeadFetch), errors.Is(err, ErrPRHeadUnavailable):
		return http.StatusServiceUnavailable, "pr-head: config temporarily unavailable"
	case errors.Is(err, ErrPRHeadBinding):
		return http.StatusUnprocessableEntity, "pr-head: ambiguous scm binding"
	default: // ErrPRHeadConfigInvalid + any unexpected
		return http.StatusUnprocessableEntity, "pr-head: config invalid"
	}
}

func (h *Handler) createPRHeadRun(
	ctx context.Context, ev pullRequestEvent, b store.PRHeadBinding, e prHeadPlanEntry,
	causeDetail json.RawMessage, delivery string, body []byte,
) fanOutOutcome {
	oc := fanOutOutcome{PipelineID: e.PipelineID, MaterialID: e.MaterialID}
	run, created, err := h.store.CreatePRHeadRun(ctx, store.CreatePRHeadRunInput{
		MaterialID:  e.MaterialID,
		ProjectID:   b.Source.ProjectID,
		RawDef:      e.HeadDef,
		Revision:    ev.HeadSHA,
		Branch:      ev.HeadRef,
		Author:      ev.Author,
		Message:     ev.Title,
		Payload:     json.RawMessage(body),
		CommittedAt: ev.At,
		TriggeredBy: "system:webhook",
		Provider:    ev.Provider,
		Delivery:    delivery,
		CauseDetail: causeDetail,
	})
	if err != nil {
		oc.Err = err
		return oc
	}
	if created {
		oc.RunID = run.RunID
		oc.RunCounter = run.Counter
		oc.ModCreated = true
	}
	return oc
}
