package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
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
type Scheduler struct {
	store    *store.Store
	sessions *grpcsrv.SessionStore
	log      *slog.Logger
	dsn      string
	tick     time.Duration
	resolver secrets.Resolver

	// Artifact download resolution. Nil artifactStore means "no artefact
	// downloads" — jobs that declare needs_artifacts will fail dispatch
	// with a clear error, matching secrets behaviour.
	artifactStore     artifacts.Store
	artifactGetURLTTL time.Duration
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
	if concurrency, _ := concurrencyFromDefinition(run.Definition); concurrency == domain.ConcurrencySerial {
		busy, err := s.store.OtherRunningRunExistsForPipeline(ctx, run.PipelineID, runID)
		if err != nil {
			s.log.Warn("scheduler: concurrency check", "run_id", runID, "err", err)
		} else if busy {
			s.log.Info("scheduler: serial pipeline busy, leaving queued",
				"run_id", runID, "pipeline_id", run.PipelineID)
			return
		}
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

		assign, err := BuildAssignment(run, job, materials, secretValues, downloads)
		if err != nil {
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

		// Labels carry the pipeline + project IDs (UUIDs) — names
		// would require an extra lookup per dispatch and most
		// dashboards either filter to one pipeline at a time OR
		// sum() over them, where the ID label disappears.
		metrics.JobsScheduled.WithLabelValues(run.PipelineID.String(), run.ProjectID.String()).Inc()
		metrics.JobsRunning.Inc()

		msg := &gocdnextv1.ServerMessage{Kind: &gocdnextv1.ServerMessage_Assign{Assign: assign}}
		if err := s.sessions.Dispatch(agentID, msg); err != nil {
			// Job is already marked running with this agent; heartbeat-timeout
			// logic (C5) will re-queue if the agent never picks it up.
			s.log.Warn("scheduler: dispatch failed, job stays running",
				"run_id", runID, "job_id", job.ID, "agent_id", agentID, "err", err)
			continue
		}

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
func (s *Scheduler) failJobWithError(ctx context.Context, job store.DispatchableJob, errMsg string) {
	if _, _, err := s.store.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: job.ID, Status: string(domain.StatusFailed), ExitCode: -1, ErrorMsg: errMsg,
	}); err != nil {
		s.log.Warn("scheduler: fail job", "job_id", job.ID, "err", err)
		return
	}
	s.log.Warn("scheduler: job failed at dispatch",
		"run_id", job.RunID, "job_id", job.ID, "job_name", job.Name, "err", errMsg)
}
