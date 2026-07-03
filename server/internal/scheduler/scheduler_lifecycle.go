package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

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
	if _, err := conn.Exec(ctx, "LISTEN "+store.SupersededRunChannel); err != nil {
		_ = conn.Close(context.Background())
		return fmt.Errorf("scheduler: LISTEN superseded: %w", err)
	}

	runCh := make(chan uuid.UUID, 32)
	supersededCh := make(chan uuid.UUID, 32)
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
				s.log.Warn("scheduler: bad notify payload", "payload", note.Payload, "channel", note.Channel)
				continue
			}
			target := runCh
			if note.Channel == store.SupersededRunChannel {
				target = supersededCh
			}
			select {
			case target <- id:
			default:
				// run_queued drops are recovered by the tick loop's re-scan.
				// run_superseded drops only defer prompt frame push — the
				// cancel_requested_at stamp + reconnect/reaper still finalize —
				// but warn so a flood is visible to the operator.
				if note.Channel == store.SupersededRunChannel {
					s.log.Warn("scheduler: superseded channel full; frame push deferred to reconnect/reaper", "run_id", id)
				}
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
		case runID := <-supersededCh:
			s.fireSupersedeEffects(ctx, runID)
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
