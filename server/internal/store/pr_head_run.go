package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/pkg/compliance"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// maxPRHeadJobRuns bounds how many job_runs one PR-head run may materialise,
// counting matrix combos + synthesized notification jobs. The head definition is
// contributor-controlled, so an unbounded matrix would be a job_runs-explosion
// DoS; the default-branch (trusted) path is unbounded as before.
const maxPRHeadJobRuns = 1000

var (
	// ErrPRHeadMaterialNotFound: the material id resolves to no pipeline/project.
	ErrPRHeadMaterialNotFound = errors.New("store: pr-head: material not found")
	// ErrPRHeadConfigDisabled: trust_same_repo_pr_config was off (or flipped off
	// concurrently — caught under the project FOR SHARE lock).
	ErrPRHeadConfigDisabled = errors.New("store: pr-head: trust_same_repo_pr_config disabled")
	// ErrPRHeadSystemManaged: the pipeline is server-owned; its definition is
	// never sourced from a PR head.
	ErrPRHeadSystemManaged = errors.New("store: pr-head: pipeline is system-managed")
	// ErrPRHeadNameMismatch: the head definition's name is not the authorized
	// pipeline's name (the head may only drive the pipeline it was matched to).
	ErrPRHeadNameMismatch = errors.New("store: pr-head: head definition name does not match the pipeline")
	// ErrPRHeadProjectMismatch: the material binds to a different project than the
	// resolver expected (defence against a mis-routed materialisation).
	ErrPRHeadProjectMismatch = errors.New("store: pr-head: material bound to a different project than expected")
	// ErrPRHeadReservedName: the head definition uses a reserved pipeline/job name.
	ErrPRHeadReservedName = errors.New("store: pr-head: reserved name in head definition")
	// ErrPRHeadNoStages: the head definition declares no stages.
	ErrPRHeadNoStages = errors.New("store: pr-head: definition has no stages")
	// ErrPRHeadTooManyJobs: matrix + policies would materialise > maxPRHeadJobRuns.
	ErrPRHeadTooManyJobs = errors.New("store: pr-head: too many job_runs after matrix + policies")
	// ErrPRHeadProfile: a runner profile is unknown or a resource exceeds its cap.
	ErrPRHeadProfile = errors.New("store: pr-head: runner profile resolution failed")
	// ErrPRHeadCluster: a referenced cluster is not registered.
	ErrPRHeadCluster = errors.New("store: pr-head: cluster resolution failed")
	// ErrPRHeadInvalidInput: a required field is empty/nil, or cause_detail is not
	// a non-null JSON object.
	ErrPRHeadInvalidInput = errors.New("store: pr-head: invalid input")
	// ErrPRHeadRerunUnsupported: a full rerun of a PR-head run is blocked — it
	// would re-materialise from the current base definition while carrying the
	// head's provenance. Job rerun within the run is unaffected. Lifted once the
	// #209 definition fence lands.
	ErrPRHeadRerunUnsupported = errors.New("store: pr-head: full rerun not supported for a PR-head run")
)

// CreatePRHeadRunInput authorises ONE materialisation from a PR head. The
// material id is the single identity — pipeline + project are derived from it
// under the lock, never trusted as independent inputs.
type CreatePRHeadRunInput struct {
	MaterialID  uuid.UUID       // PRIMARY identity; pipeline + project derived under lock
	ProjectID   uuid.UUID       // REQUIRED: the project the resolver bound this material to; verified under lock
	RawDef      domain.Pipeline // the parsed head definition (RAW, pre-policy)
	Revision    string          // head SHA — the single provenance source (dedup + config_revision + digest)
	Branch      string          // head ref (required; a NULL branch would break the dedup unique key)
	Author      string
	Message     string
	Payload     json.RawMessage
	CommittedAt time.Time
	TriggeredBy string
	Provider    string          // required, store-set into cause_detail (e.g. "github")
	Delivery    string          // required, store-set into cause_detail (webhook delivery id)
	CauseDetail json.RawMessage // PR metadata (pr_number, pr_title, ...); config_* keys are store-set.
	// The lane is derived from CauseDetail's pr_number (like the push/webhook
	// path); the cause is always pull_request. Neither is a caller input.
}

// CreatePRHeadRun authorises and materialises a single run from a PR head
// definition, atomically. It fetches NOTHING and parses no SCM — the caller
// resolves + parses the head `.gocdnext/` before this. Inside one transaction it
// acquires the compliance lock then the project row (FOR SHARE), re-checks the
// envelope guards, applies policies to the RAW definition, caps the materialised
// job_runs, inserts the dedup modification + the run, and returns. The bool is
// false (with a nil error) when the modification already existed (a replay): no
// run is created and insertRunRowsTx is never called.
func (s *Store) CreatePRHeadRun(ctx context.Context, in CreatePRHeadRunInput) (RunCreated, bool, error) {
	// Reserved-name guards on the RAW definition — pure, before opening the tx.
	if err := compliance.RejectReservedNames(in.RawDef); err != nil {
		return RunCreated{}, false, fmt.Errorf("%w: %v", ErrPRHeadReservedName, err)
	}
	if compliance.IsReservedPipelineName(in.RawDef.Name) {
		return RunCreated{}, false, fmt.Errorf("%w: pipeline %q", ErrPRHeadReservedName, in.RawDef.Name)
	}
	if len(in.RawDef.Stages) == 0 {
		return RunCreated{}, false, ErrPRHeadNoStages
	}
	// Required provenance. Empty branch would also break the dedup unique key
	// (material_id, revision, branch).
	if in.ProjectID == uuid.Nil || in.Revision == "" || in.Branch == "" || in.Provider == "" || in.Delivery == "" {
		return RunCreated{}, false, ErrPRHeadInvalidInput
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RunCreated{}, false, fmt.Errorf("store: pr-head: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// Lock order: compliance (shared) BEFORE the project row, matching
	// ApplyProject, so create-run and disable/apply are linearised without an
	// inverted-order deadlock.
	if err := lockComplianceShared(ctx, tx); err != nil {
		return RunCreated{}, false, fmt.Errorf("store: pr-head: compliance lock: %w", err)
	}

	// Derive material -> pipeline -> project UNDER a FOR SHARE lock on the project.
	lc, err := q.LockPRHeadRunContext(ctx, pgUUID(in.MaterialID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunCreated{}, false, ErrPRHeadMaterialNotFound
	}
	if err != nil {
		return RunCreated{}, false, fmt.Errorf("store: pr-head: lock context: %w", err)
	}

	// Envelope guards for this single materialisation. The project binding is
	// mandatory: the resolver knows the project, so the material MUST derive to it.
	if fromPgUUID(lc.ProjectID) != in.ProjectID {
		return RunCreated{}, false, ErrPRHeadProjectMismatch
	}
	if !lc.TrustSameRepoPrConfig {
		return RunCreated{}, false, ErrPRHeadConfigDisabled
	}
	if lc.SystemManaged {
		return RunCreated{}, false, ErrPRHeadSystemManaged
	}
	if in.RawDef.Name != lc.PipelineName {
		return RunCreated{}, false, fmt.Errorf("%w: head %q vs pipeline %q", ErrPRHeadNameMismatch, in.RawDef.Name, lc.PipelineName)
	}

	// Base-authorized envelope: the head drives ONLY the executable graph (stages,
	// jobs, services, variables). Materials, concurrency, supersede and
	// notifications stay the BASE's — a PR can't change them from its `.gocdnext/`
	// (else it could cancel prior runs, drop base serialisation, or suppress a
	// mandatory notification).
	var baseDef domain.Pipeline
	if err := json.Unmarshal(lc.BaseDefinitionRaw, &baseDef); err != nil {
		return RunCreated{}, false, fmt.Errorf("store: pr-head: decode base definition: %w", err)
	}
	candidate := baseDef
	candidate.Stages = in.RawDef.Stages
	candidate.Jobs = in.RawDef.Jobs
	candidate.Services = in.RawDef.Services
	candidate.Variables = in.RawDef.Variables

	// Apply the project's current policies to the CANDIDATE (base envelope + head
	// graph) IN-TX.
	policies, err := policiesForProject(ctx, q, lc.ProjectID)
	if err != nil {
		return RunCreated{}, false, fmt.Errorf("store: pr-head: load policies: %w", err)
	}
	effective := compliance.ApplyPolicies(candidate, policies)

	// Resolve runner-profile image/resource defaults + caps and validate cluster
	// references on the SAME tx querier — AFTER policies (so policy-injected jobs
	// are covered) and BEFORE the cap + snapshot. Without this the head snapshot
	// would carry unresolved resources and bypass the admin profile cap at
	// dispatch (which reads resources straight from the snapshot).
	pipes := []*domain.Pipeline{&effective}
	if err := resolveProfilesQ(ctx, q, pipes); err != nil {
		return RunCreated{}, false, fmt.Errorf("%w: %v", ErrPRHeadProfile, err)
	}
	if err := resolveClustersQ(ctx, q, pipes); err != nil {
		return RunCreated{}, false, fmt.Errorf("%w: %v", ErrPRHeadCluster, err)
	}

	// Cap the materialised job_runs (matrix + synthesized notification jobs)
	// BEFORE any write — the head matrix is attacker-controllable.
	if prHeadJobCount(effective, lc.ProjectNotifications) > maxPRHeadJobRuns {
		return RunCreated{}, false, fmt.Errorf("%w (> %d)", ErrPRHeadTooManyJobs, maxPRHeadJobRuns)
	}

	// Snapshot bytes are produced from the FINAL effective definition only.
	runDef, err := effectiveDefFromPipeline(effective)
	if err != nil {
		return RunCreated{}, false, fmt.Errorf("store: pr-head: %w", err)
	}

	// Dedup ledger + run in the SAME tx (atomicity): a later failure rolls the
	// modification back too, so a retry of the same SHA can recover.
	modRes, err := insertModificationQ(ctx, q, Modification{
		MaterialID:  in.MaterialID,
		Revision:    in.Revision,
		Branch:      in.Branch,
		Author:      in.Author,
		Message:     in.Message,
		Payload:     in.Payload,
		CommittedAt: in.CommittedAt,
	})
	if err != nil {
		return RunCreated{}, false, err
	}
	if !modRes.Created {
		// Replay of an already-seen SHA: no run. Commit (nothing was created)
		// and report created=false.
		if err := tx.Commit(ctx); err != nil {
			return RunCreated{}, false, fmt.Errorf("store: pr-head: commit (dedup): %w", err)
		}
		return RunCreated{}, false, nil
	}

	causeDetail, err := prHeadCauseDetail(in, modRes.ID, runDef.bytes)
	if err != nil {
		return RunCreated{}, false, fmt.Errorf("%w: %v", ErrPRHeadInvalidInput, err)
	}
	revisions, _ := json.Marshal(map[string]any{
		in.MaterialID.String(): map[string]string{"revision": in.Revision, "branch": in.Branch},
	})

	var pendingAuditEmits []AuditEmit
	result, err := s.insertRunRowsTx(ctx, tx, q, runRowsSpec{
		PipelineID:    fromPgUUID(lc.PipelineID),
		Def:           runDef,
		ProjectNotifs: lc.ProjectNotifications,
		Cause:         string(domain.CausePullRequest),
		CauseDetail:   causeDetail,
		Revisions:     revisions,
		TriggeredBy:   in.TriggeredBy,
		Ref:           prHeadLaneRef(causeDetail, in.Branch),
	}, &pendingAuditEmits)
	if err != nil {
		return RunCreated{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return RunCreated{}, false, fmt.Errorf("store: pr-head: commit: %w", err)
	}
	for _, emit := range pendingAuditEmits {
		if _, err := s.EmitAuditEvent(ctx, emit); err != nil {
			slog.Warn("store: pr-head: audit emit failed",
				"err", err, "target_id", emit.TargetID, "action", emit.Action)
		}
	}
	return result, true, nil
}

// isPRHeadCauseDetail reports whether a run's cause_detail marks it a PR-head
// run (config_source=pr_head) — used to block a full rerun of such a run.
func isPRHeadCauseDetail(causeDetail []byte) bool {
	if len(causeDetail) == 0 {
		return false
	}
	var d struct {
		ConfigSource string `json:"config_source"`
	}
	if err := json.Unmarshal(causeDetail, &d); err != nil {
		return false
	}
	return d.ConfigSource == "pr_head"
}

// prHeadCauseDetail merges the resolver's PR metadata with the store-set
// provenance keys. It accepts ONLY a non-null JSON OBJECT for CauseDetail (a
// "null" or a non-object is an error, not silently dropped — the map would be
// nil and the assignments would panic). The store-set keys (provider, delivery,
// material_id, modification_id, config_*) are written LAST so the caller's
// metadata can't spoof them; config_revision is the single head SHA.
func prHeadCauseDetail(in CreatePRHeadRunInput, modID int64, defBytes []byte) (json.RawMessage, error) {
	detail := map[string]any{}
	if len(in.CauseDetail) > 0 {
		if err := json.Unmarshal(in.CauseDetail, &detail); err != nil {
			return nil, fmt.Errorf("cause_detail must be a json object: %w", err)
		}
		if detail == nil { // a literal "null" unmarshals to a nil map
			return nil, errors.New("cause_detail must be a non-null json object")
		}
	}
	detail["provider"] = in.Provider
	detail["delivery"] = in.Delivery
	detail["material_id"] = in.MaterialID.String()
	detail["modification_id"] = modID
	detail["config_source"] = "pr_head"
	detail["config_revision"] = in.Revision
	sum := sha256.Sum256(defBytes)
	detail["config_digest"] = hex.EncodeToString(sum[:])
	out, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// prHeadLaneRef derives the supersede lane the same way the push/webhook path
// does: pr:<number> from the cause_detail's pr_number, falling back to the head
// branch when the number is missing/malformed.
func prHeadLaneRef(causeDetail json.RawMessage, branch string) string {
	var detail map[string]any
	if err := json.Unmarshal(causeDetail, &detail); err == nil {
		if pr, ok := prLaneRef(detail["pr_number"]); ok {
			return pr
		}
	}
	return branch
}

// prHeadJobCount returns the number of job_runs a run of `def` would
// materialise: the sum over jobs of matrix cardinality (empty matrix = 1 combo,
// mirroring expandMatrix) PLUS the synthesized notification jobs (the def's own
// notifications, or the inherited project set when the def declares none). The
// value saturates at maxPRHeadJobRuns+1, so a pathological matrix can neither
// overflow int64 nor be undercounted.
func prHeadJobCount(def domain.Pipeline, projectNotifs []byte) int64 {
	total := int64(0)
	for _, job := range def.Jobs {
		combos := int64(1)
		for _, values := range job.Matrix {
			if len(values) == 0 {
				continue
			}
			combos = mulSatPRHead(combos, int64(len(values)))
		}
		total += combos
		if total > maxPRHeadJobRuns {
			return maxPRHeadJobRuns + 1
		}
	}
	total += int64(prHeadNotifCount(def, projectNotifs))
	if total > maxPRHeadJobRuns {
		return maxPRHeadJobRuns + 1
	}
	return total
}

// mulSatPRHead multiplies, saturating at maxPRHeadJobRuns+1 so the running
// product never overflows int64 (both operands stay small until the cap).
func mulSatPRHead(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > (maxPRHeadJobRuns+1)/b {
		return maxPRHeadJobRuns + 1
	}
	return a * b
}

// prHeadNotifCount mirrors insertRunRowsTx's effective-notifications rule: an
// explicit `notifications:` on the def (including an empty opt-out) wins;
// otherwise the project-level set is inherited.
func prHeadNotifCount(def domain.Pipeline, projectNotifs []byte) int {
	if def.Notifications != nil {
		return len(def.Notifications)
	}
	if len(projectNotifs) == 0 {
		return 0
	}
	var ns []domain.Notification
	if err := json.Unmarshal(projectNotifs, &ns); err != nil {
		return 0
	}
	return len(ns)
}
