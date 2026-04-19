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
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
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
	pumpDone := make(chan struct{})

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
		case <-ticker.C:
			s.drainQueued(ctx)
		}
	}
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

	dispatched := 0
	for _, job := range jobs {
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

		assign, err := BuildAssignment(run, job, materials, secretValues)
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
