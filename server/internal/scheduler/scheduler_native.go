package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/deploysvc"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// deployGuardOutcome is what beginDeployGuard tells its caller to do.
type deployGuardOutcome int

const (
	guardProceed deployGuardOutcome = iota // no guard needed (supersede off) or acquired
	guardSkip                              // blocked/busy: queue_reason set, skip this tick
	guardError                             // fail-closed: dispatch nothing, retry
)

// beginDeployGuard runs the #97 supersede backstop for a deploy job's env: for
// supersede != off it takes the lane-env advisory lock and checks the newer-gate-pass
// marker. On guardProceed it returns the held guard (nil when supersede is off) — the
// caller must Release it. On guardSkip / guardError the guard is already released (or
// was never taken) and the caller skips the job. Shared by the native takeover and the
// plugin/agent dispatch path so both enforce supersede identically.
func (s *Scheduler) beginDeployGuard(ctx context.Context, run store.RunForDispatch, job store.DispatchableJob, env string) (*store.DeploymentRevisionGuard, deployGuardOutcome) {
	laneMode, err := supersedeModeFromDefinition(run.Definition)
	if err != nil {
		s.log.Warn("scheduler: read supersede mode for deployment guard",
			"run_id", run.ID, "job_id", job.ID, "err", err)
		return nil, guardError
	}
	if laneMode != domain.SupersedeBranch && laneMode != domain.SupersedePipeline {
		return nil, guardProceed // supersede off: no guard
	}
	guard, status, err := s.store.BeginDeploymentRevisionGuard(ctx, store.BeginDeploymentRevisionGuardInput{
		PipelineID:  run.PipelineID,
		Counter:     run.Counter,
		Ref:         run.Ref,
		LaneMode:    laneMode,
		Environment: env,
	})
	if err != nil {
		metrics.SupersedeBackstopErrors.Inc()
		s.log.Warn("scheduler: deployment guard",
			"run_id", run.ID, "job_id", job.ID, "environment", env, "err", err)
		return nil, guardError
	}
	switch status {
	case store.DeploymentRevisionGuardLockBusy:
		metrics.SupersedeLockBusy.Inc()
		if err := s.store.SetActiveRunQueueReason(ctx, run.ID, "supersede-lock-busy:"+env); err != nil {
			s.log.Warn("scheduler: set supersede lock-busy queue_reason",
				"run_id", run.ID, "job_id", job.ID, "environment", env, "err", err)
		}
		// Debug, not Info: fires every tick a lane-env stays contended.
		s.log.Debug("scheduler: deployment guard lock busy",
			"run_id", run.ID, "job_id", job.ID, "environment", env)
		return nil, guardSkip
	case store.DeploymentRevisionGuardBlocked:
		if err := s.store.SetActiveRunQueueReason(ctx, run.ID, "supersede-blocked:"+env); err != nil {
			s.log.Warn("scheduler: set supersede blocked queue_reason",
				"run_id", run.ID, "job_id", job.ID, "environment", env, "err", err)
		}
		s.log.Info("scheduler: deployment blocked by newer gate-pass",
			"run_id", run.ID, "job_id", job.ID, "environment", env)
		return nil, guardSkip
	}
	return guard, guardProceed
}

// tryNativeDeploy attempts the native deploy takeover (ADR-0001, Model A) for a deploy
// job, BEFORE any agent is required. Returns true when the dispatch loop should
// `continue` (took over / lost the CAS / blocked-or-busy / fail-closed / terminal
// version error); false to fall through to the plugin/agent path (no registered
// target). The supersede guard is held across the whole takeover so the #97 TOCTOU
// guarantee matches the plugin path.
func (s *Scheduler) tryNativeDeploy(ctx context.Context, run store.RunForDispatch, job store.DispatchableJob, jobDef domain.Job) bool {
	env := jobDef.Deploy.Environment

	guard, outcome := s.beginDeployGuard(ctx, run, job, env)
	if outcome != guardProceed {
		return true // blocked / busy / fail-closed — nothing dispatched this tick
	}
	release := func() {
		if guard != nil {
			if err := guard.Release(ctx); err != nil {
				s.log.Warn("scheduler: release native deploy guard",
					"run_id", run.ID, "job_id", job.ID, "err", err)
			}
		}
	}

	version, revision, err := s.resolveNativeDeployMarker(ctx, run, job, jobDef)
	if err != nil {
		release()
		// Terminal version errors match the plugin path's contract (#39) — fail the
		// job loud rather than retrying an unresolvable config forever. A
		// non-correlatable explicit version (native-only) is terminal too.
		if errors.Is(err, ErrDeployVersionEmpty) || errors.Is(err, ErrDeployVersionUnresolved) ||
			errors.Is(err, ErrDeployVersionNotCorrelatable) {
			s.failJobWithError(ctx, job, err.Error())
		} else {
			s.log.Warn("scheduler: native deploy marker",
				"run_id", run.ID, "job_id", job.ID, "err", err)
		}
		return true
	}

	res, err := s.native.TakeOver(ctx, deploysvc.NativeDeployInput{
		ProjectID:   run.ProjectID,
		RunID:       run.ID,
		JobRunID:    job.ID,
		Environment: env,
		Version:     version,
		Revision:    revision,
		Now:         time.Now(),
	})
	release() // guard held across TakeOver's tx + Sync; release now
	if err != nil {
		// Fail-closed: a real registry/DB failure — dispatch nothing, retry next tick.
		s.log.Warn("scheduler: native deploy takeover",
			"run_id", run.ID, "job_id", job.ID, "environment", env, "err", err)
		return true
	}
	switch res.Decision {
	case deploysvc.DecisionNative:
		s.log.Info("scheduler: native deploy dispatched",
			"run_id", run.ID, "job_id", job.ID, "environment", env,
			"application", res.Application, "sync_mode", res.SyncMode)
		return true
	case deploysvc.DecisionSkip:
		return true // lost the dispatch CAS to another tick/replica
	default: // DecisionFallback — no registered target
		return false
	}
}

// resolveNativeDeployMarker resolves the display version AND the git correlation
// revision for a native deploy, WITHOUT needing an agent (so the takeover can precede
// agent-finding). version mirrors the plugin path exactly (shared resolver); revision
// is the FULL commit SHA ArgoCD reports — the watch correlates + wins success on it,
// so it must be the full SHA, not the short-sha display version (see
// correlationRevision for the full/short/expand rules). A native deploy that can't be
// tied to a git SHA fails terminally: an empty default version →
// ErrDeployVersionEmpty, a non-correlatable explicit version →
// ErrDeployVersionNotCorrelatable. So this never returns an unpinned ("") revision for
// a run that reaches dispatch.
func (s *Scheduler) resolveNativeDeployMarker(ctx context.Context, run store.RunForDispatch, job store.DispatchableJob, jobDef domain.Job) (version, revision string, err error) {
	needsOutputs, matrixNeedsOutputs, nErr := s.buildNeedsOutputs(ctx, run.ID, job)
	if nErr != nil {
		return "", "", nErr
	}
	var def domain.Pipeline
	if uErr := json.Unmarshal(run.Definition, &def); uErr != nil {
		return "", "", fmt.Errorf("scheduler: decode pipeline: %w", uErr)
	}
	dims := buildMatrixDims(def, matrixNeedsOutputs)
	ciVars := buildCIVars(run, job.Name)
	version, err = resolveDeployMarkerVersion(job.Name, jobDef, needsOutputs, matrixNeedsOutputs, dims, ciVars)
	if err != nil {
		return "", "", err
	}
	// Version is the display/ledger string as-is. Revision is the FULL SHA ArgoCD
	// reports — resolved from the explicit deploy.version when it's a git SHA, else the
	// run's commit; a non-correlatable explicit version fails the deploy (terminal).
	explicit := strings.TrimSpace(jobDef.Deploy.Version) != ""
	revision, cerr := correlationRevision(job.Name, version, explicit, ciVars["CI_COMMIT_SHA"])
	if cerr != nil {
		return "", "", cerr
	}
	return version, revision, nil
}
