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
)

// CreatePRHeadRunInput authorises ONE materialisation from a PR head. The
// material id is the single identity — pipeline + project are derived from it
// under the lock, never trusted as independent inputs.
type CreatePRHeadRunInput struct {
	MaterialID  uuid.UUID       // PRIMARY identity; pipeline + project derived under lock
	RawDef      domain.Pipeline // the parsed head definition (RAW, pre-policy)
	Revision    string          // head SHA (also the modification revision)
	Branch      string          // head ref
	Author      string
	Message     string
	Payload     json.RawMessage
	CommittedAt time.Time
	TriggeredBy string
	Ref         string          // supersede lane key (e.g. "pr:<n>")
	Cause       string          // defaults to pull_request
	CauseDetail json.RawMessage // PR metadata from the resolver; config_* keys are store-set
	// ConfigRevision is the head SHA the config was fetched at (for cause_detail).
	ConfigRevision string
	// ExpectedProjectID, when non-nil, is verified against the material's owning
	// project under the lock — a resolver binding check. uuid.Nil skips it.
	ExpectedProjectID uuid.UUID
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

	// Envelope guards for this single materialisation.
	if in.ExpectedProjectID != uuid.Nil && fromPgUUID(lc.ProjectID) != in.ExpectedProjectID {
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

	// Apply the project's current policies to the RAW head definition IN-TX.
	policies, err := policiesForProject(ctx, q, lc.ProjectID)
	if err != nil {
		return RunCreated{}, false, fmt.Errorf("store: pr-head: load policies: %w", err)
	}
	effective := compliance.ApplyPolicies(in.RawDef, policies)

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

	cause := in.Cause
	if cause == "" {
		cause = string(domain.CausePullRequest)
	}
	causeDetail := prHeadCauseDetail(in.CauseDetail, in.MaterialID, modRes.ID, in.ConfigRevision, runDef.bytes)
	revisions, _ := json.Marshal(map[string]any{
		in.MaterialID.String(): map[string]string{"revision": in.Revision, "branch": in.Branch},
	})

	var pendingAuditEmits []AuditEmit
	result, err := s.insertRunRowsTx(ctx, tx, q, runRowsSpec{
		PipelineID:    fromPgUUID(lc.PipelineID),
		Def:           runDef,
		ProjectNotifs: lc.ProjectNotifications,
		Cause:         cause,
		CauseDetail:   causeDetail,
		Revisions:     revisions,
		TriggeredBy:   in.TriggeredBy,
		Ref:           in.Ref,
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

// prHeadCauseDetail merges the resolver's PR metadata with the store-set config
// provenance keys. The config_* keys are written LAST so they can't be spoofed
// by whatever the caller put in CauseDetail.
func prHeadCauseDetail(base json.RawMessage, materialID uuid.UUID, modID int64, configRev string, defBytes []byte) json.RawMessage {
	detail := map[string]any{}
	if len(base) > 0 {
		_ = json.Unmarshal(base, &detail)
	}
	detail["material_id"] = materialID.String()
	detail["modification_id"] = modID
	detail["config_source"] = "pr_head"
	detail["config_revision"] = configRev
	sum := sha256.Sum256(defBytes)
	detail["config_digest"] = hex.EncodeToString(sum[:])
	out, _ := json.Marshal(detail)
	return out
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
