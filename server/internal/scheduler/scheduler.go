package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// DefaultTickInterval is how often the scheduler rescans queued runs as a
// backstop for missed LISTEN notifications (after a driver reconnect, say).
const DefaultTickInterval = 15 * time.Second

// Scheduler turns queued runs into JobAssignments delivered to agents.
// GitTokenSource resolves a short-lived clone token for a git URL.
// Returns ("", nil) when no token is available (no App installed on
// the repo, no PAT registered, public repo) so the caller falls
// through to an unauthenticated clone — same behaviour as before
// the App was wired. vcs.Registry implements this against the
// active GitHub AppClient; tests can pass a static stub.
type GitTokenSource interface {
	TokenForGitURL(ctx context.Context, repoURL string) (token string, err error)
}

type Scheduler struct {
	store    *store.Store
	sessions *grpcsrv.SessionStore
	log      *slog.Logger
	dsn      string
	tick     time.Duration
	resolver secrets.Resolver

	// cipher unseals runner profile secrets at dispatch time. Nil-ok:
	// dispatching a job whose profile carries secrets without a cipher
	// fails the dispatch (rather than silently losing the values).
	cipher *crypto.Cipher

	// Artifact download resolution. Nil artifactStore means "no artefact
	// downloads" — jobs that declare needs_artifacts will fail dispatch
	// with a clear error, matching secrets behaviour.
	artifactStore     artifacts.Store
	artifactGetURLTTL time.Duration

	// gitTokens, when set, mints per-repo clone tokens that get
	// embedded in the MaterialCheckout URL + echoed into LogMasks
	// so private-repo clones succeed without the operator wiring a
	// per-project secret. nil = current PAT-only behaviour.
	gitTokens GitTokenSource

	// idTokens, when set, mints per-job OIDC JWTs for jobs that
	// declare id_tokens: in YAML. nil = feature off; such jobs then
	// fail dispatch loud (configuration error, same contract as
	// secrets/artifacts above).
	idTokens IDTokenMinter

	// checks closes a superseded run's GitHub check (the JobResult path that
	// normally reports run completion is skipped on supersede). nil = GitHub
	// checks disabled; the effects listener guards on it. Interface (not the
	// concrete *checks.Reporter) so tests can spy.
	checks checksReporter
}

// checksReporter is the slice of the GitHub checks reporter the supersede effects
// need. *checks.Reporter satisfies it. Kept minimal + local so the scheduler
// doesn't hard-depend on the checks package's full surface and tests can fake it.
type checksReporter interface {
	ReportRunCompleted(ctx context.Context, runID uuid.UUID, status string)
}

func (s *Scheduler) DispatchRun(ctx context.Context, runID uuid.UUID) {
	s.dispatchRun(ctx, runID)
}

func (s *Scheduler) dispatchRun(ctx context.Context, runID uuid.UUID) {
	run, err := s.store.GetRunForDispatch(ctx, runID)
	if err != nil {
		s.log.Warn("scheduler: get run", "run_id", runID, "err", err)
		return
	}

	// Serial pipelines queue behind an already-running run of the
	// same pipeline. Dispatch resumes via the periodic drainQueued
	// tick (at most one tick of latency once the predecessor ends)
	// or via the OnSessionReady hook when an agent reconnects —
	// both paths self-heal without a dedicated "run finished"
	// notification channel. A follow-up could wire one for
	// sub-tick latency on hot serial deploys.
	//
	// When the gate fires, stamp queue_reason='serial-busy:<id>' so
	// the runs list / detail can render "waiting on #N" (issue #4
	// path #2). The stamp is best-effort: if it fails, we still
	// leave the run queued — the operator just loses the surface
	// for THIS tick. The next tick will re-stamp.
	if concurrency, _ := concurrencyFromDefinition(run.Definition); concurrency == domain.ConcurrencySerial {
		predecessor, busy, err := s.store.OtherRunningRunForPipeline(ctx, run.PipelineID, runID)
		if err != nil {
			s.log.Warn("scheduler: concurrency check", "run_id", runID, "err", err)
		} else if busy {
			reason := "serial-busy:" + predecessor.String()
			if err := s.store.SetRunQueueReason(ctx, runID, reason); err != nil {
				s.log.Warn("scheduler: set queue_reason failed",
					"run_id", runID, "predecessor", predecessor, "err", err)
			}
			s.log.Info("scheduler: serial pipeline busy, leaving queued",
				"run_id", runID, "pipeline_id", run.PipelineID,
				"predecessor", predecessor)
			return
		}
	}

	// Past the gate — clear any prior queue_reason so a run that was
	// previously stamped 'serial-busy:X' (and is now proceeding
	// because X finished) doesn't carry the stale message into its
	// running window. Idempotent in SQL (IS NOT NULL guard), so the
	// common path (no prior stamp) is cheap.
	if err := s.store.ClearRunQueueReason(ctx, runID); err != nil {
		s.log.Warn("scheduler: clear queue_reason failed", "run_id", runID, "err", err)
	}

	jobs, err := s.store.ListDispatchableJobs(ctx, runID)
	if err != nil {
		s.log.Warn("scheduler: list jobs", "run_id", runID, "err", err)
		return
	}
	if len(jobs) == 0 {
		return
	}

	materials, err := s.store.ListPipelineMaterials(ctx, run.PipelineID)
	if err != nil {
		s.log.Warn("scheduler: list materials", "pipeline_id", run.PipelineID, "err", err)
		return
	}

	// Snapshot of every job_run in this run, keyed by name. The
	// needs-satisfaction gate below consults this once per candidate
	// to decide whether all upstreams are terminally green. Loaded
	// ONCE per tick (not per candidate) so a stage with N dependent
	// jobs costs 1 query rather than N — see buildJobStatusMap.
	//
	// Stale-by-a-tick is fine: a job whose upstream completes mid-
	// tick will hit the gate on the next NOTIFY-driven tick (the
	// CompleteJob cascade fires job_completed, the scheduler's
	// drainQueued loop picks it up). Worst case is one tick of
	// latency — same shape as the existing serial-busy retry path.
	statusList, err := s.store.ListJobStatusForRun(ctx, runID)
	if err != nil {
		s.log.Warn("scheduler: list job status", "run_id", runID, "err", err)
		return
	}
	statusMap := buildJobStatusMap(statusList)

	// Cache the user-stage outcome so we don't re-query it for
	// every synth notification job in the same tick. nil = not
	// queried yet; lookup is lazy + at-most-once per tick.
	var userOutcome *store.UserStageOutcome
	userOutcomeFor := func() (store.UserStageOutcome, error) {
		if userOutcome != nil {
			return *userOutcome, nil
		}
		o, err := s.store.GetRunUserStageOutcome(ctx, runID)
		if err != nil {
			return store.UserStageOutcome{}, err
		}
		userOutcome = &o
		return o, nil
	}

	dispatched := 0
	for _, job := range jobs {
		// Synth notification jobs: evaluate the `on:` trigger
		// against the user-stage outcome. Non-matching entries
		// get skipped in-place (not canceled — skipped means "not
		// attempted by design") so the notifications stage can
		// close once the last dispatch or skip lands. Spec can
		// come from the pipeline YAML OR inherited from the
		// project — resolveNotificationSpec checks both.
		if idx, isNotif := domain.NotificationIndexFromName(job.Name); isNotif {
			notif, ok := resolveNotificationSpec(run, idx)
			if !ok {
				s.log.Warn("scheduler: notification spec missing", "run_id", runID, "idx", idx)
				continue
			}
			o, err := userOutcomeFor()
			if err != nil {
				s.log.Warn("scheduler: user outcome lookup", "run_id", runID, "err", err)
				continue
			}
			if !store.NotificationTriggerMatches(notif.On, o) {
				if _, _, err := s.store.SkipNotificationJob(ctx, job.ID); err != nil {
					s.log.Warn("scheduler: skip notification", "job_id", job.ID, "err", err)
				}
				continue
			}
		}

		// needs-satisfaction gate: a job that declares `needs:` cannot
		// dispatch until every named upstream has reached terminal-
		// succeeded. Before this gate existed, jobs in the same stage
		// declaring inter-job needs would dispatch concurrently — the
		// downstream's `resolveArtifactDeps` would then fail because
		// the upstream hadn't produced artefacts yet, and the
		// downstream would be marked `failed` permanently (issue
		// (`build` needing `types-generate` in the same stage).
		//
		// Gate runs BEFORE the agent / secrets / artifact lookups so
		// a blocked job doesn't consume a session slot or DB round-
		// trip just to be re-queued. Synth notification jobs have
		// empty `needs` (not parseable from YAML for them), so
		// `needsSatisfied(nil, _)` returns Ok=true trivially.
		//
		// Branches on the verdict:
		//   - UpstreamTerminal=true: fail the downstream with the
		//     reason (failJobNeedsUnmet); status='failed' so the
		//     cascade counts it toward run failure — silent-green
		//     is closed even if a snapshot drift bypassed parser
		//     validation.
		//   - UpstreamTerminal=false: leave queued; the always-
		//     notify in connect.go fires on every job completion
		//     so the next tick re-evaluates with fresh status map.
		if check := needsSatisfied(job.Needs, statusMap); !check.Ok {
			if check.UpstreamTerminal {
				s.failJobNeedsUnmet(ctx, job, check.Detail)
			} else {
				s.log.Info("scheduler: needs not yet satisfied, leaving queued",
					"run_id", runID, "job_id", job.ID, "job_name", job.Name,
					"waiting_on", check.Detail,
					"blockers", summarizeNeeds(job.Needs, statusMap))
			}
			continue
		}

		requiredTags, tagsErr := JobTagsFromDefinition(run.Definition, job.Name)
		if tagsErr != nil {
			s.log.Warn("scheduler: read job tags", "job_id", job.ID, "err", tagsErr)
			continue
		}
		agentID, ok := s.sessions.FindIdleWithTags(requiredTags)
		if !ok {
			s.log.Info("scheduler: no matching idle agent, leaving queued",
				"run_id", runID, "job_id", job.ID, "job_name", job.Name,
				"required_tags", requiredTags)
			// Other jobs might have different tag requirements — keep trying.
			continue
		}

		secretValues, secretErr := s.resolveJobSecrets(ctx, run, job.Name)
		if secretErr != nil {
			s.failJobWithError(ctx, job, fmt.Sprintf("secrets: %v", secretErr))
			continue
		}

		downloads, depErr := s.resolveArtifactDeps(ctx, run, job.Name)
		if depErr != nil {
			s.failJobWithError(ctx, job, fmt.Sprintf("artifact deps: %v", depErr))
			continue
		}

		profile, profErr := s.resolveProfile(ctx, run, job.Name)
		if profErr != nil {
			s.failJobWithError(ctx, job, fmt.Sprintf("runner profile: %v", profErr))
			continue
		}

		// Resolve a clone token for each git material URL before
		// building the assignment. Best-effort: a missing source
		// or a public repo returns "" and the clone runs unauth
		// just like before.
		cloneTokens := s.resolveCloneTokens(ctx, materials)

		// Build the NeedsOutputs table for this candidate (issue
		// #10). Scoped to job.Needs — typical 1-3 names — so the
		// query is cheap even on runs with 50+ jobs. Runs AFTER
		// the gate so `needsSatisfied` guarantees all upstream
		// rows are terminal-success: outputs were written in the
		// same UPDATE that flipped status, so the read here sees
		// a consistent snapshot. Empty needs → skip the query
		// entirely (most jobs).
		//
		// Matrix-expanded upstreams (issue #21) flow into
		// matrixNeedsOutputs; bare-ref upstreams flow into
		// needsOutputs. BuildAssignment routes the substitution
		// to the right table based on whether the ref body has
		// a `matrix[...]` segment.
		needsOutputs, matrixNeedsOutputs, nErr := s.buildNeedsOutputs(ctx, runID, job)
		if nErr != nil {
			s.failJobWithError(ctx, job, fmt.Sprintf("needs outputs: %v", nErr))
			continue
		}

		// OIDC id_tokens (keyless cloud auth). Minted fresh per
		// dispatch — a rerun gets a new jti/exp. Failure here is a
		// CONFIGURATION error (issuer off, key unavailable): the next
		// tick reproduces it identically, so terminalise loud instead
		// of leaving the job queued forever. NEVER dispatch a job
		// without a token it declared.
		idTokens, idErr := s.mintIDTokens(ctx, run, job)
		if idErr != nil {
			s.failJobWithError(ctx, job, fmt.Sprintf("id_tokens: %v", idErr))
			continue
		}

		// Managed cluster: resolve the kubeconfig to inject as
		// PLUGIN_KUBECONFIG. A declared-but-unresolvable cluster
		// (deleted / project not authorized) fails the dispatch rather
		// than shipping a deploy at the wrong (or no) cluster.
		clusterKubeconfig, clusterMasks, clErr := s.resolveClusterKubeconfig(ctx, run, job)
		if clErr != nil {
			s.failJobWithError(ctx, job, fmt.Sprintf("cluster: %v", clErr))
			continue
		}

		assign, deployTarget, err := BuildAssignment(run, job, materials, secretValues, downloads, profile, cloneTokens, needsOutputs, matrixNeedsOutputs, idTokens, clusterKubeconfig, clusterMasks)
		if err != nil {
			// Unresolved needs refs (issue #10) are CONFIGURATION
			// errors — the next tick will see the exact same
			// state and produce the exact same error. Without
			// this branch the job sits queued forever and the
			// operator chases a "why isn't this dispatching"
			// mystery. Terminalise with the original error message
			// so the run-log surfaces which ref + which alias.
			if errors.Is(err, ErrNeedsRefUnresolved) {
				s.failJobWithError(ctx, job, fmt.Sprintf("needs outputs: %v", err))
				continue
			}
			// Deploy version that can't be resolved (empty default, or
			// any unresolvable ref — CI var absent this run, needs shape
			// that slipped the parser) is a per-run config error too —
			// terminalise instead of retrying forever (#39).
			if errors.Is(err, ErrDeployVersionEmpty) || errors.Is(err, ErrDeployVersionUnresolved) {
				s.failJobWithError(ctx, job, err.Error())
				continue
			}
			s.log.Warn("scheduler: build assignment", "job_id", job.ID, "err", err)
			continue
		}

		// releaseDeployGuard drops the lane-env advisory lock; called explicitly on
		// EVERY exit of this iteration (lost-CAS, revision error, dispatch error,
		// success). It is idempotent (nils the guard). It is deliberately NOT a
		// per-iteration `defer` — that would defer to FUNCTION return and hold the
		// lock across later iterations. A panic between acquire and release isn't
		// handled here because there is no recover() anywhere in the scheduler path
		// (verified), so a panic crashes the process and the OS closes the pooled
		// conn — releasing the session-level lock. If a recover is ever added
		// upstack, wrap the guard-holding span in a closure with its own defer.
		var deployGuard *store.DeploymentRevisionGuard
		releaseDeployGuard := func() {
			if deployGuard == nil {
				return
			}
			if err := deployGuard.Release(ctx); err != nil {
				s.log.Warn("scheduler: release deployment guard",
					"run_id", runID, "job_id", job.ID, "err", err)
			}
			deployGuard = nil
		}
		if deployTarget != nil && !job.DeployRollback {
			laneMode, err := supersedeModeFromDefinition(run.Definition)
			if err != nil {
				s.log.Warn("scheduler: read supersede mode for deployment guard",
					"run_id", runID, "job_id", job.ID, "err", err)
				continue
			}
			if laneMode == domain.SupersedeBranch || laneMode == domain.SupersedePipeline {
				guard, status, err := s.store.BeginDeploymentRevisionGuard(ctx, store.BeginDeploymentRevisionGuardInput{
					PipelineID:  run.PipelineID,
					Counter:     run.Counter,
					Ref:         run.Ref,
					LaneMode:    laneMode,
					Environment: deployTarget.Environment,
				})
				if err != nil {
					// Fail-closed guard/infra error: the deploy is NOT dispatched.
					metrics.SupersedeBackstopErrors.Inc()
					s.log.Warn("scheduler: deployment guard",
						"run_id", runID, "job_id", job.ID, "environment", deployTarget.Environment, "err", err)
					continue
				}
				if status == store.DeploymentRevisionGuardLockBusy {
					metrics.SupersedeLockBusy.Inc()
					reason := "supersede-lock-busy:" + deployTarget.Environment
					if err := s.store.SetActiveRunQueueReason(ctx, runID, reason); err != nil {
						s.log.Warn("scheduler: set supersede lock-busy queue_reason",
							"run_id", runID, "job_id", job.ID, "environment", deployTarget.Environment, "err", err)
					}
					// Debug, not Info: fires every tick a lane-env stays contended,
					// so a higher level would spam the log (the metric is the signal).
					s.log.Debug("scheduler: deployment guard lock busy",
						"run_id", runID, "job_id", job.ID, "environment", deployTarget.Environment)
					continue
				}
				if status == store.DeploymentRevisionGuardBlocked {
					reason := "supersede-blocked:" + deployTarget.Environment
					if err := s.store.SetActiveRunQueueReason(ctx, runID, reason); err != nil {
						s.log.Warn("scheduler: set supersede blocked queue_reason",
							"run_id", runID, "job_id", job.ID, "environment", deployTarget.Environment, "err", err)
					}
					s.log.Info("scheduler: deployment blocked by newer gate-pass",
						"run_id", runID, "job_id", job.ID, "environment", deployTarget.Environment)
					continue
				}
				deployGuard = guard
			}
		}

		assigned, ok, err := s.store.AssignJob(ctx, job.ID, agentID)
		if err != nil {
			releaseDeployGuard()
			s.log.Warn("scheduler: assign", "job_id", job.ID, "err", err)
			continue
		}
		if !ok {
			// Lost optimistic race with another scheduler tick / replica.
			releaseDeployGuard()
			continue
		}

		// Deployment tracking (#39): record the in_progress revision
		// HERE — after AssignJob stamped (running, attempt) but BEFORE
		// DispatchAssignment hands the job to the agent. This window is
		// the only safe spot: the agent doesn't have the job yet, so no
		// JobResult can race in and call FinalizeDeploymentRevision
		// against a revision that doesn't exist (a fast job would
		// otherwise finalise 0 rows, then this create would leave an
		// orphan in_progress that never reads as "current"). This is now
		// fail-closed: if the revision can't be recorded, rollback AssignJob
		// and retry later rather than dispatching an untrackable deploy. If
		// the dispatch below fails, we delete this revision in the rollback.
		var deployRevID uuid.UUID
		if deployTarget != nil {
			deployRevID, err = s.createDeployRevision(ctx, run, job, assigned.Attempt, deployTarget)
			if err != nil {
				releaseDeployGuard()
				s.rollbackUndispatchedAssignment(ctx, runID, job, agentID, assigned, uuid.Nil,
					"deploy revision create failed", err)
				continue
			}
		}

		msg := &gocdnextv1.ServerMessage{Kind: &gocdnextv1.ServerMessage_Assign{Assign: assign}}
		// Atomic record-and-dispatch: the snapshot stamp and the
		// channel enqueue MUST land on the SAME session. A two-call
		// version (record then dispatch) had a TOCTOU window where
		// a successor Register could swap the session in between —
		// the record landed on the obsolete session, the dispatch
		// went to the new one, and the new one had no assignment
		// for the job → eventual JobResult got dropped as "no
		// assignment". DispatchAssignment holds the SessionStore
		// mutex for both operations on whichever session is
		// current at lock-acquire time.
		if err := s.sessions.DispatchAssignment(agentID, msg, assigned.ID, assigned.Attempt); err != nil {
			releaseDeployGuard()
			s.rollbackUndispatchedAssignment(ctx, runID, job, agentID, assigned, deployRevID,
				"dispatch failed", err)
			continue
		}
		releaseDeployGuard()

		// Metrics fire AFTER successful dispatch so a dispatch
		// failure (rolled back above) doesn't leak counters that
		// nobody decrements. Labels carry pipeline + project IDs
		// (UUIDs) — names would require an extra lookup per
		// dispatch and most dashboards either filter to one
		// pipeline at a time OR sum() over them, where the ID
		// label disappears.
		metrics.JobsScheduled.WithLabelValues(run.PipelineID.String(), run.ProjectID.String()).Inc()
		metrics.JobsRunning.Inc()

		if err := s.store.MarkStageRunning(ctx, job.StageRunID); err != nil {
			s.log.Warn("scheduler: mark stage running", "err", err)
		}
		dispatched++
		s.log.Info("scheduler: dispatched",
			"run_id", runID, "job_id", assigned.ID, "job_name", assigned.Name,
			"agent_id", agentID)
		// (Deploy revision was created above, before DispatchAssignment.)
	}

	if dispatched > 0 {
		if err := s.store.MarkRunRunning(ctx, runID); err != nil {
			s.log.Warn("scheduler: mark run running", "err", err)
		}
	}
}

// IsNoIdleAgent is a small helper callers can use to distinguish a benign
// "try later" from real errors.
func IsNoIdleAgent(err error) bool {
	return errors.Is(err, grpcsrv.ErrNoSession)
}

func (s *Scheduler) rollbackUndispatchedAssignment(ctx context.Context, runID uuid.UUID, job store.DispatchableJob, agentID uuid.UUID, assigned store.AssignedJob, deployRevID uuid.UUID, reason string, cause error) {
	// Frame never reached an agent. Roll the row back via snapshot CAS so it goes
	// back to (queued, NULL) and the scheduler retries on the next tick. Metrics:
	// NOTHING was incremented yet (JobsScheduled/JobsRunning fire only on
	// successful dispatch), so there's nothing to undo here.
	//
	// If a deploy revision was created, delete it too: no deploy happened, and
	// leaving it would ghost the timeline and collide with the next dispatch's
	// create on the (job_run, attempt) unique index when the retry reuses the same
	// attempt.
	if deployRevID != uuid.Nil {
		if derr := s.store.DeleteDeploymentRevision(ctx, deployRevID); derr != nil {
			s.log.Warn("scheduler: delete deploy revision after undispatched assignment",
				"job_id", job.ID, "revision_id", deployRevID, "reason", reason, "err", derr)
		}
	}
	runIDForNotify, ok, undoErr := s.store.UnassignJob(ctx, job.ID, agentID, assigned.Attempt)
	switch {
	case undoErr != nil:
		s.log.Warn("scheduler: undispatched assignment rollback errored",
			"run_id", runID, "job_id", job.ID, "agent_id", agentID,
			"reason", reason, "cause", cause, "unassign_err", undoErr)
	case ok:
		if nerr := s.store.NotifyRunQueued(ctx, runIDForNotify); nerr != nil {
			s.log.Warn("scheduler: undispatched assignment notify failed",
				"run_id", runIDForNotify, "reason", reason, "err", nerr)
		}
		s.log.Warn("scheduler: undispatched assignment rolled back to queued",
			"run_id", runID, "job_id", job.ID, "agent_id", agentID,
			"reason", reason, "err", cause)
	default:
		// Snapshot didn't match — a concurrent path already claimed this row in a
		// different state. Leave it.
		s.log.Warn("scheduler: undispatched assignment snapshot stale — leaving row alone",
			"run_id", runID, "job_id", job.ID, "agent_id", agentID,
			"reason", reason, "err", cause)
	}
}
