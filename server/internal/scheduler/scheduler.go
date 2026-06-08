package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
	"github.com/gocdnext/gocdnext/server/internal/store"
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
}

// New wires the scheduler. dsn is used for a dedicated LISTEN connection —
// pgxpool can't hold one exclusively for waits.
func New(s *store.Store, sessions *grpcsrv.SessionStore, log *slog.Logger, dsn string) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		store:    s,
		sessions: sessions,
		log:      log,
		dsn:      dsn,
		tick:     DefaultTickInterval,
	}
}

// WithGitTokens wires the per-repo clone-token source. nil disables
// the feature (clones run unauthenticated, same as pre-wire days).
func (s *Scheduler) WithGitTokens(src GitTokenSource) *Scheduler {
	s.gitTokens = src
	return s
}

// resolveCloneTokens asks the gitTokens source for a fresh installation
// token for every git material in the list. Errors are logged and
// folded to empty so a partial failure (one repo unreachable, App not
// installed on a sibling repo, etc.) doesn't take the whole dispatch
// down — the failing clone bubbles up with the standard 128 exit code
// and the operator chases the dependency on the right repo. Returns a
// map keyed by material id so BuildAssignment can plumb each token to
// the matching MaterialCheckout.
func (s *Scheduler) resolveCloneTokens(ctx context.Context, materials []store.Material) map[string]string {
	if s.gitTokens == nil {
		return nil
	}
	out := map[string]string{}
	for _, m := range materials {
		if m.Type != string(domain.MaterialGit) {
			continue
		}
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			continue
		}
		if cfg.URL == "" {
			continue
		}
		tok, err := s.gitTokens.TokenForGitURL(ctx, cfg.URL)
		if err != nil {
			s.log.Warn("scheduler: clone token mint failed",
				"material_id", m.ID, "url", cfg.URL, "err", err)
			continue
		}
		if tok != "" {
			out[m.ID.String()] = tok
		}
	}
	return out
}

// WithTickInterval overrides the backstop tick. Mainly for tests.
func (s *Scheduler) WithTickInterval(d time.Duration) *Scheduler {
	if d > 0 {
		s.tick = d
	}
	return s
}

// WithSecretResolver plugs in the backend that supplies secret values at
// dispatch time. nil / omitted means "no secrets subsystem" and jobs that
// reference secrets will fail dispatch with a clear error instead of
// silently running with empty env values. The default DBResolver lives in
// internal/secrets; future Vault/AWS/GCP adapters implement the same
// contract.
func (s *Scheduler) WithSecretResolver(r secrets.Resolver) *Scheduler {
	s.resolver = r
	return s
}

// WithCipher plugs in the AEAD used to unseal runner profile secrets.
// Same cipher the rest of the platform uses (project secrets, global
// secrets, auth providers). Nil-ok at construction; resolveProfileEnv
// fails the dispatch when the job's profile actually carries secrets
// and the cipher is missing.
func (s *Scheduler) WithCipher(c *crypto.Cipher) *Scheduler {
	s.cipher = c
	return s
}

// WithArtifactStore plugs in the backend used to sign download URLs for
// `needs_artifacts`. ttl <= 0 falls back to 30 minutes (agents under
// load can sit in queue for a while before they pull, so don't be
// stingy). Must be set or intra-run downloads refuse dispatch.
func (s *Scheduler) WithArtifactStore(store artifacts.Store, ttl time.Duration) *Scheduler {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	s.artifactStore = store
	s.artifactGetURLTTL = ttl
	return s
}

// Run blocks until ctx is canceled, consuming `run_queued` notifications and
// draining the queued-run backlog every tick.
func (s *Scheduler) Run(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, s.dsn)
	if err != nil {
		return fmt.Errorf("scheduler: connect: %w", err)
	}

	if _, err := conn.Exec(ctx, "LISTEN "+store.RunQueuedChannel); err != nil {
		_ = conn.Close(context.Background())
		return fmt.Errorf("scheduler: LISTEN: %w", err)
	}

	runCh := make(chan uuid.UUID, 32)
	// drainCh is the single-producer signal: on an agent register,
	// or a tick, we coalesce into one drain without blocking the
	// signaller. Buffer of 1 because two back-to-back drains add
	// nothing — the second finds the same (post-drain) state.
	drainCh := make(chan struct{}, 1)
	pumpDone := make(chan struct{})

	s.sessions.SetOnSessionReady(func() {
		select {
		case drainCh <- struct{}{}:
		default:
		}
	})
	// Tear the hook down when we exit — a future ctx-cancelled
	// scheduler must not keep pulling the now-unbuffered channel.
	defer s.sessions.SetOnSessionReady(nil)

	// Notify pump → runCh. Owns exclusive access to conn while ctx is live.
	// Must exit before we Close(conn); otherwise pgx's internal conn state is
	// written and read concurrently.
	go func() {
		defer close(pumpDone)
		for {
			note, err := conn.WaitForNotification(ctx)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("scheduler: listen error (pump exiting)", "err", err)
				}
				return
			}
			id, err := uuid.Parse(note.Payload)
			if err != nil {
				s.log.Warn("scheduler: bad notify payload", "payload", note.Payload)
				continue
			}
			select {
			case runCh <- id:
			default:
				// Channel full: drop, the tick loop will pick it up.
			}
		}
	}()

	s.log.Info("scheduler started", "tick", s.tick, "channel", store.RunQueuedChannel)

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	// Prime on startup: catch up with anything queued before we started.
	s.drainQueued(ctx)

	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopping")
			<-pumpDone
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = conn.Close(closeCtx)
			cancel()
			return nil
		case runID := <-runCh:
			s.dispatchRun(ctx, runID)
		case <-drainCh:
			// Agent came online — re-try every queued run so jobs
			// stop sitting around for up to a tick interval just
			// because nothing was listening when they were created.
			s.drainQueued(ctx)
		case <-ticker.C:
			s.drainQueued(ctx)
			s.refreshQueueDepth(ctx)
		}
	}
}

// refreshQueueDepth updates the gocdnext_queue_depth gauge from a
// single aggregate read. Best-effort: a pgx blip on this read just
// keeps the previous gauge value, the next tick will recover.
func (s *Scheduler) refreshQueueDepth(ctx context.Context) {
	snap, err := s.store.GetQueueDepth(ctx)
	if err != nil {
		s.log.Warn("scheduler: queue depth", "err", err)
		return
	}
	metrics.QueueDepth.WithLabelValues("queued").Set(float64(snap.QueuedRuns))
	metrics.QueueDepth.WithLabelValues("pending").Set(float64(snap.PendingJobs))
}

func (s *Scheduler) drainQueued(ctx context.Context) {
	ids, err := s.store.ListQueuedRunIDs(ctx)
	if err != nil {
		s.log.Warn("scheduler: list queued", "err", err)
		return
	}
	for _, id := range ids {
		s.dispatchRun(ctx, id)
	}
}

// DispatchRun is exported so tests can exercise it directly without the
// LISTEN loop. The live path calls it internally.
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
		// Matrix-ambiguity enforced in buildNeedsOutputs: if any
		// referenced name expanded to >1 row, the candidate is
		// failed with a clear "ambiguous matrix" error citing the
		// matrix keys, BEFORE BuildAssignment ever sees the data.
		needsOutputs, nErr := s.buildNeedsOutputs(ctx, runID, job)
		if nErr != nil {
			s.failJobWithError(ctx, job, fmt.Sprintf("needs outputs: %v", nErr))
			continue
		}

		assign, err := BuildAssignment(run, job, materials, secretValues, downloads, profile, cloneTokens, needsOutputs)
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
			s.log.Warn("scheduler: build assignment", "job_id", job.ID, "err", err)
			continue
		}

		assigned, ok, err := s.store.AssignJob(ctx, job.ID, agentID)
		if err != nil {
			s.log.Warn("scheduler: assign", "job_id", job.ID, "err", err)
			continue
		}
		if !ok {
			// Lost optimistic race with another scheduler tick / replica.
			continue
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
			// Frame never reached an agent. Roll the row back via
			// snapshot CAS so it goes back to (queued, NULL) and
			// the scheduler retries on the next tick. Metrics:
			// NOTHING was incremented yet (JobsScheduled/JobsRunning
			// fire only on successful dispatch below), so there's
			// nothing to undo here.
			runIDForNotify, ok, undoErr := s.store.UnassignJob(ctx, job.ID, agentID, assigned.Attempt)
			switch {
			case undoErr != nil:
				s.log.Warn("scheduler: dispatch failed AND unassign errored",
					"run_id", runID, "job_id", job.ID, "agent_id", agentID,
					"dispatch_err", err, "unassign_err", undoErr)
			case ok:
				if nerr := s.store.NotifyRunQueued(ctx, runIDForNotify); nerr != nil {
					s.log.Warn("scheduler: dispatch undo notify failed",
						"run_id", runIDForNotify, "err", nerr)
				}
				s.log.Warn("scheduler: dispatch failed, row rolled back to queued",
					"run_id", runID, "job_id", job.ID, "agent_id", agentID, "err", err)
			default:
				// Snapshot didn't match — a concurrent path already
				// claimed this row in a different state. Leave it.
				s.log.Warn("scheduler: dispatch failed, snapshot stale on unassign — leaving row alone",
					"run_id", runID, "job_id", job.ID, "agent_id", agentID, "err", err)
			}
			continue
		}

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

// resolveProfile pulls the runner profile referenced by the job
// (if any) and returns the full ResolvedProfile — merged env, secret
// values for LogMasks, and the k8s scheduling hints (NodeSelector +
// Tolerations) that the agent engine pipes into the pod spec. A job
// without a profile returns an empty ResolvedProfile — the fast path
// stays free.
//
// Profile lookup is by name from the job definition; missing profile
// fails the dispatch with a clear error so the operator notices a
// rename/typo instead of silently shipping without the env.
func (s *Scheduler) resolveProfile(ctx context.Context, run store.RunForDispatch, jobName string) (store.ResolvedProfile, error) {
	jobDef, err := jobDefFromDefinition(run.Definition, jobName)
	if err != nil {
		return store.ResolvedProfile{}, err
	}
	if jobDef.Profile == "" {
		return store.ResolvedProfile{}, nil
	}
	resolved, err := s.store.ResolveProfileByName(ctx, s.cipher, jobDef.Profile)
	if err != nil {
		return store.ResolvedProfile{}, fmt.Errorf("profile %q: %w", jobDef.Profile, err)
	}
	return resolved, nil
}

// resolveJobSecrets reads the declared secret names off the pipeline
// definition snapshot, then asks the configured Resolver for their values.
// Returns an empty map when the job has no secrets. Fails when a job
// references secrets but no Resolver is configured, or when a declared name
// isn't present in the backend — both are user-visible pipeline mistakes.
func (s *Scheduler) resolveJobSecrets(ctx context.Context, run store.RunForDispatch, jobName string) (map[string]string, error) {
	names, err := JobSecretsFromDefinition(run.Definition, jobName)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	if s.resolver == nil {
		return nil, fmt.Errorf("secret %q declared but no secrets backend is configured on this server", names[0])
	}
	resolved, err := s.resolver.Resolve(ctx, run.ProjectID, names)
	if err != nil {
		return nil, err
	}
	// Every declared name must be present; Resolver implementations silently
	// drop unknown names, so we diff here for a precise error.
	var missing []string
	for _, n := range names {
		if _, ok := resolved[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("secrets not set on project: %v", missing)
	}
	return resolved, nil
}

// resolveArtifactDeps turns the job's needs_artifacts entries into a
// list of signed-URL download tickets. Fails when: no artifact backend
// is configured but the job declares deps, an upstream job produced
// zero ready artefacts (matching the optional paths filter), or
// signing errors. Empty return for a job with no deps.
func (s *Scheduler) resolveArtifactDeps(ctx context.Context, run store.RunForDispatch, jobName string) ([]*gocdnextv1.ArtifactDownload, error) {
	deps, err := JobArtifactDepsFromDefinition(run.Definition, jobName)
	if err != nil {
		return nil, err
	}
	if len(deps) == 0 {
		return nil, nil
	}
	if s.artifactStore == nil {
		return nil, fmt.Errorf("needs_artifacts declared but no artifact backend is configured on this server")
	}

	out := make([]*gocdnextv1.ArtifactDownload, 0)
	for _, dep := range deps {
		sourceRunID, err := s.resolveDepRunID(ctx, run, dep)
		if err != nil {
			return nil, err
		}
		rows, err := s.store.ListReadyArtifactsByRunAndJob(ctx, sourceRunID, dep.FromJob, dep.Paths)
		if err != nil {
			return nil, fmt.Errorf("lookup artefacts from %q: %w", dep.FromJob, err)
		}
		if len(rows) == 0 {
			scope := "same run"
			if dep.FromPipeline != "" {
				scope = fmt.Sprintf("upstream run of pipeline %q", dep.FromPipeline)
			}
			if len(dep.Paths) == 0 {
				return nil, fmt.Errorf("no ready artefacts found from job %q (%s)", dep.FromJob, scope)
			}
			return nil, fmt.Errorf("no ready artefacts from job %q matching paths %v (%s)", dep.FromJob, dep.Paths, scope)
		}
		dest := dep.Dest
		if dest == "" {
			dest = "./"
		}
		for _, a := range rows {
			signed, err := s.artifactStore.SignedGetURL(ctx, a.StorageKey, s.artifactGetURLTTL)
			if err != nil {
				return nil, fmt.Errorf("sign get url for %q: %w", a.Path, err)
			}
			out = append(out, &gocdnextv1.ArtifactDownload{
				Path:          a.Path,
				StorageKey:    a.StorageKey,
				GetUrl:        signed.URL,
				Dest:          dest,
				ContentSha256: a.ContentSHA256,
				FromJob:       a.JobName,
			})
		}
	}
	return out, nil
}

// resolveDepRunID picks the run_id whose artefacts back this
// particular `needs_artifacts` entry. Empty FromPipeline = current
// run (intra). Set FromPipeline = upstream run that triggered this
// run (fanout). Validates that the upstream is indeed the named
// pipeline so a typo surfaces as a clear error instead of "no
// artefacts found".
func (s *Scheduler) resolveDepRunID(ctx context.Context, run store.RunForDispatch, dep domain.ArtifactDep) (uuid.UUID, error) {
	if dep.FromPipeline == "" {
		return run.ID, nil
	}
	upstream, err := s.store.GetRunUpstreamContext(ctx, run.ID)
	if err != nil {
		return uuid.Nil, err
	}
	if upstream.UpstreamRunID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("needs_artifacts references upstream pipeline %q but this run has no upstream (cause is webhook/manual)", dep.FromPipeline)
	}
	if upstream.UpstreamPipeline != dep.FromPipeline {
		return uuid.Nil, fmt.Errorf("needs_artifacts references upstream pipeline %q but this run was triggered by %q", dep.FromPipeline, upstream.UpstreamPipeline)
	}
	return upstream.UpstreamRunID, nil
}

// failJobWithError marks a still-queued job as failed (with cascade to
// stage/run via store.CompleteJob). Called when dispatch-time resolution
// fails — e.g. a declared secret isn't set on the project. CompleteJob's
// WHERE clause accepts both queued and running, so we don't need to flip
// to running first just to fail it.
//
// ExpectedAgentID is uuid.Nil here on purpose: a queued job has
// agent_id IS NULL, and CompleteJobRun's `IS NOT DISTINCT FROM`
// predicate matches NULL with NULL. If a scheduler tick raced the
// agent (somehow the row got AssignJob'd between our list and our
// fail), this NULL-expected guard makes our fail no-op via ErrNoRows
// — which is the correct outcome, since a job that's been picked up
// by a live agent should not be failed by the dispatch path.
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
func (s *Scheduler) buildNeedsOutputs(ctx context.Context, runID uuid.UUID, job store.DispatchableJob) (NeedsOutputs, error) {
	if len(job.Needs) == 0 {
		return nil, nil
	}
	rows, err := s.store.ListJobOutputsForRun(ctx, runID, job.Needs)
	if err != nil {
		return nil, fmt.Errorf("list job outputs: %w", err)
	}
	return groupNeedsOutputs(rows)
}

// groupNeedsOutputs is the pure validation/grouping helper extracted
// from buildNeedsOutputs so the matrix-ambiguity policy can be tested
// without a DB round-trip. Public-package-local; not exported.
//
// Matrix policy (Kleber's #6 invariant for Layer 6): if a referenced
// name expanded to >1 row (matrix job with >1 instance), error LOUD
// listing the matrix keys involved. The downstream substitution
// can't disambiguate; explicit per-row selector
// `${{ needs.X.matrix[key].outputs.Y }}` is roadmap for #10 follow-up.
// We refuse to fold ambiguous data into the substitution input rather
// than letting the downstream pick "any matrix row" silently.
//
// Non-success upstream rows are dropped silently — the
// needsSatisfied gate already blocks dispatch when an upstream
// isn't terminal-success, so this is defence in depth. A downstream
// ref against a dropped row falls through to substituteNeedsRefs'
// "did not produce the named output" message, which is the right
// UX (the row's not here as far as substitution can tell).
func groupNeedsOutputs(rows []store.JobOutputs) (NeedsOutputs, error) {
	grouped := make(map[string][]store.JobOutputs)
	for _, r := range rows {
		grouped[r.Name] = append(grouped[r.Name], r)
	}
	out := make(NeedsOutputs, len(grouped))
	for name, group := range grouped {
		if len(group) > 1 {
			keys := make([]string, 0, len(group))
			for _, g := range group {
				if g.MatrixKey == "" {
					keys = append(keys, "<empty>")
				} else {
					keys = append(keys, g.MatrixKey)
				}
			}
			return nil, fmt.Errorf(
				"upstream job %q has %d matrix instances [%s] — `${{ needs.%s.outputs.* }}` is ambiguous. "+
					"v1 supports outputs only from non-matrix upstreams; explicit per-row selector is roadmap",
				name, len(group), strings.Join(keys, ", "), name)
		}
		row := group[0]
		if row.Status != string(domain.StatusSuccess) {
			continue
		}
		out[name] = row.Outputs
	}
	return out, nil
}

func (s *Scheduler) failJobWithError(ctx context.Context, job store.DispatchableJob, errMsg string) {
	if _, _, err := s.store.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        job.ID,
		Status:          string(domain.StatusFailed),
		ExitCode:        -1,
		ErrorMsg:        errMsg,
		ExpectedAgentID: uuid.Nil,
		ExpectedAttempt: job.Attempt,
	}); err != nil {
		s.log.Warn("scheduler: fail job", "job_id", job.ID, "err", err)
		return
	}
	s.log.Warn("scheduler: job failed at dispatch",
		"run_id", job.RunID, "job_id", job.ID, "job_name", job.Name, "err", errMsg)
}
