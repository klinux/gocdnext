package grpcsrv

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/checks"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/logarchive"
	"github.com/gocdnext/gocdnext/server/internal/logstream"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// AgentService implements gocdnextv1.AgentServiceServer. It owns the
// server-side lifecycle of an agent: authenticate on Register, hold a session,
// exchange events on Connect.
type AgentService struct {
	gocdnextv1.UnimplementedAgentServiceServer

	store            *store.Store
	sessions         *SessionStore
	log              *slog.Logger
	heartbeatSeconds int32

	// autoRegisterToken enables on-demand agent row creation: when
	// non-empty AND a Register RPC arrives with name=<unknown> +
	// token=<this value>, the server inserts a fresh row keyed by
	// the name + hash(token). Empty disables — agents must be
	// pre-provisioned. Set via WithAutoRegisterToken.
	autoRegisterToken string

	// artifactStore + artifactTTL are optional: nil store means the
	// server was started without a configured backend and artifact
	// uploads will return Unimplemented. artifactDefaultRetention is
	// what we stamp into `expires_at` when the YAML doesn't override.
	artifactStore            artifacts.Store
	artifactPutURLTTL        time.Duration
	artifactGetURLTTL        time.Duration
	artifactDefaultRetention time.Duration

	// checksReporter: optional; nil means "don't report back to GitHub
	// when a run terminates". Set via WithChecksReporter.
	checksReporter *checks.Reporter

	// logBroker: optional in-process fan-out so the HTTP SSE handler
	// can tail live log lines without polling the DB. nil means
	// "feature off" — handleLogLine still persists, just skips the
	// publish. Set via WithLogBroker.
	logBroker *logstream.Broker

	// logArchiver: optional cold-archive worker. When set + the
	// effective per-job policy resolves to true, handleJobResult
	// enqueues the just-completed job for archiving. nil means
	// "feature off" — log_lines stays in the partitioned heap.
	// Set via WithLogArchiver.
	logArchiver      *logarchive.Archiver
	logArchivePolicy domain.LogArchivePolicy

	// jobRunIDCache memoises jobID → runID lookups so the log hot
	// path avoids a DB round-trip per line. Job IDs are stable for
	// their lifetime (no re-assignment) so the entry never goes
	// stale; the worst case is the process growing one uuid entry
	// per job seen since last restart, which is negligible.
	jobRunIDCache sync.Map

	// registerFenceMaxAttempts caps how many times a job that gets
	// caught by the register-fence path can be re-queued before
	// being failed. Mirrors the reaper's MaxAttempts default (3)
	// so a job whose agent flap-restarts repeatedly fails after
	// the same total tries, regardless of which path (heartbeat-
	// stale vs re-register) detected the orphan. Override via
	// WithRegisterFenceMaxAttempts for tests / operators tuning
	// the policy globally.
	registerFenceMaxAttempts int32
}

// DefaultRegisterFenceMaxAttempts is duplicated from
// scheduler.DefaultReaperMaxAttempts to avoid an import cycle
// (scheduler depends on grpcsrv). Keep the two in sync.
const DefaultRegisterFenceMaxAttempts int32 = 3

// NewAgentService wires the service. heartbeatSeconds is the cadence the server
// asks the agent to use; zero means "let the agent pick" — we default to 30.
func NewAgentService(s *store.Store, sessions *SessionStore, log *slog.Logger, heartbeatSeconds int32) *AgentService {
	if log == nil {
		log = slog.Default()
	}
	if heartbeatSeconds <= 0 {
		heartbeatSeconds = 30
	}
	return &AgentService{
		store:                    s,
		sessions:                 sessions,
		log:                      log,
		heartbeatSeconds:         heartbeatSeconds,
		registerFenceMaxAttempts: DefaultRegisterFenceMaxAttempts,
	}
}

// WithRegisterFenceMaxAttempts lets the operator (or tests) tune
// how many times the register-fence path will re-queue an orphan
// before failing it. Zero or negative leaves the default in place
// — there's no "disable the fence" mode because that mode is just
// the old "jobs stuck running forever" bug.
func (a *AgentService) WithRegisterFenceMaxAttempts(n int32) *AgentService {
	if n > 0 {
		a.registerFenceMaxAttempts = n
	}
	return a
}

// WithArtifactStore enables the RequestArtifactUpload + cache RPCs.
// Without this the RPCs return Unimplemented — tests that don't
// exercise artifacts don't need to wire a backend. TTLs of zero fall
// back to 15 min for the PUT URL, 30 min for the GET URL (cache
// download can queue a bit), and 30 days for the retention stamp.
func (a *AgentService) WithArtifactStore(st artifacts.Store, putURLTTL, getURLTTL, retention time.Duration) *AgentService {
	if putURLTTL <= 0 {
		putURLTTL = 15 * time.Minute
	}
	if getURLTTL <= 0 {
		getURLTTL = 30 * time.Minute
	}
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	a.artifactStore = st
	a.artifactPutURLTTL = putURLTTL
	a.artifactGetURLTTL = getURLTTL
	a.artifactDefaultRetention = retention
	return a
}

// WithAutoRegisterToken turns on opt-in agent auto-registration.
// When set, a Register RPC with an unknown agent name + a token
// matching this value creates the row before completing the
// registration. Subsequent registers for the same name validate
// against the token hash now stored on the row, so this token
// only matters on first contact.
//
// Trust model: the operator wraps the agent fleet in a single
// shared token (the same Helm secret each agent pod ships) and
// the server accepts any pod presenting it. Multi-tenant
// deployments where agents shouldn't share a token must keep
// auto-register OFF and pre-provision via SQL/CLI.
func (a *AgentService) WithAutoRegisterToken(token string) *AgentService {
	a.autoRegisterToken = token
	return a
}

// WithChecksReporter plugs the GitHub Checks API reporter that will
// be called when a run reaches terminal state. Nil-safe: if the
// server was started without an App configured, callers pass nil
// and the handle-result path just skips the report.
func (a *AgentService) WithChecksReporter(r *checks.Reporter) *AgentService {
	a.checksReporter = r
	return a
}

// WithLogBroker enables live fan-out of log lines to SSE
// subscribers. Nil-safe: without a broker the log path still
// persists to Postgres, it just skips the publish (clients fall
// back to polling).
func (a *AgentService) WithLogBroker(b *logstream.Broker) *AgentService {
	a.logBroker = b
	return a
}

// WithLogArchiver wires the cold-archive worker + the global policy
// the handler uses to resolve "should this job ship its logs?". The
// policy is folded with each job's project flag at terminal time,
// so the per-job decision tracks live admin changes without a
// service restart.
func (a *AgentService) WithLogArchiver(arch *logarchive.Archiver, policy domain.LogArchivePolicy) *AgentService {
	a.logArchiver = arch
	a.logArchivePolicy = policy
	return a
}

// Register validates the agent's pre-provisioned token, updates its metadata
// and returns an ephemeral session id to use on Connect.
func (a *AgentService) Register(ctx context.Context, req *gocdnextv1.RegisterRequest) (*gocdnextv1.RegisterResponse, error) {
	if req.GetAgentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id is required")
	}
	if req.GetToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "token is required")
	}

	agent, err := a.store.FindAgentByName(ctx, req.GetAgentId())
	if errors.Is(err, store.ErrAgentNotFound) {
		// Auto-register opt-in: when the operator configured a
		// shared registration token AND the agent presents it,
		// we mint the row on the spot. Constant-time compare so
		// "wrong token" and "no token configured" are
		// indistinguishable from a timing-attack perspective.
		if a.autoRegisterToken != "" &&
			subtle.ConstantTimeCompare([]byte(req.GetToken()), []byte(a.autoRegisterToken)) == 1 {
			created, cerr := a.store.CreateAgent(
				ctx, req.GetAgentId(), req.GetToken(),
				req.GetTags(), req.GetCapacity(),
			)
			if cerr != nil {
				a.log.Error("agent auto-register: create failed",
					"agent_id", req.GetAgentId(), "err", cerr)
				return nil, status.Error(codes.Internal, "internal error")
			}
			a.log.Info("agent auto-registered",
				"agent_id", req.GetAgentId(), "agent_uuid", created.ID)
			agent = created
		} else {
			a.log.Warn("agent register: unknown agent", "agent_id", req.GetAgentId())
			return nil, status.Error(codes.NotFound, "agent not registered")
		}
	} else if err != nil {
		a.log.Error("agent register: lookup failed", "agent_id", req.GetAgentId(), "err", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	if !store.VerifyToken(req.GetToken(), agent.TokenHash) {
		a.log.Warn("agent register: bad token", "agent_id", req.GetAgentId())
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}

	// Orphan-recovery + session-publish sequence. Order matters; each
	// step addresses a specific race (issue #4 + 4 review rounds):
	//
	//   1. RevokeForAgent: kill the OLD session and set its
	//      supersededByRegister flag so the Connect-handler defer on
	//      that stream knows not to MarkAgentOffline (which would
	//      clobber the fresh online stamp we set below). After this,
	//      latestByAg has no entry for this agent — the scheduler
	//      can't dispatch to either old or new yet.
	//
	//   2. ReclaimAgentJobs: requeue / fail-at-max every running row
	//      attributed to this agent. Snapshot-validating CAS inside
	//      the store rejects rows that a concurrent path already
	//      redispatched. notify=false — the wake-up is coalesced and
	//      fired after CreateSession (step 4).
	//
	//   3. MarkAgentOnline: flip agents.status='online' BEFORE
	//      publishing the new session. If this DB UPDATE fails we
	//      bail with no in-memory state created — the old session
	//      is gone (step 1), but agents.status keeps whatever value
	//      it had. The reaper's stale-last_seen_at path is the
	//      eventual safety net.
	//
	//   4. CreateSession: publishes the new session in latestByAg
	//      AND fires onReady. Both happen AFTER MarkAgentOnline
	//      committed, so the scheduler can never observe a live
	//      session for an agents.status='offline' row. (Prior
	//      ordering — CreateSession before MarkAgentOnline — let
	//      onReady wake the scheduler while the DB still said
	//      offline; the reaper would then reclaim the very job the
	//      scheduler was about to dispatch.)
	//
	//   5. NotifyRunQueued: coalesced wake-up for every requeued
	//      run. By this point the new session exists AND the agents
	//      row is online, so FindIdle picks it cleanly.
	//
	// Error policy: log + continue inside reclaim (per-row failures
	// are tolerable; reaper retries them). MarkAgentOnline is the
	// one hard gate — if it fails we have no inconsistent in-memory
	// state to clean up.
	a.sessions.RevokeForAgent(agent.ID)

	var notifyRunIDs []uuid.UUID
	if results, err := a.store.ReclaimAgentJobs(ctx, agent.ID, a.registerFenceMaxAttempts); err != nil {
		a.log.Warn("agent register: fence failed",
			"agent_id", req.GetAgentId(), "agent_uuid", agent.ID, "err", err)
	} else if len(results) > 0 {
		var requeued, failed, skipped, errored int
		seenRuns := make(map[uuid.UUID]struct{}, len(results))
		for _, r := range results {
			switch {
			case r.Err != nil:
				errored++
				a.log.Warn("agent register: fence entry error",
					"agent_uuid", agent.ID, "job_id", r.JobRunID, "err", r.Err)
			case r.Action == store.ReclaimActionRequeued:
				requeued++
				if _, dup := seenRuns[r.RunID]; !dup {
					seenRuns[r.RunID] = struct{}{}
					notifyRunIDs = append(notifyRunIDs, r.RunID)
				}
			case r.Action == store.ReclaimActionFailed:
				failed++
			default:
				skipped++
			}
		}
		a.log.Info("agent register: fence reclaimed orphans",
			"agent_id", req.GetAgentId(), "agent_uuid", agent.ID,
			"requeued", requeued, "failed", failed,
			"skipped", skipped, "errors", errored)
	}

	upd := store.RegisterUpdate{
		Version:  req.GetVersion(),
		OS:       req.GetOs(),
		Arch:     req.GetArch(),
		Tags:     req.GetTags(),
		Capacity: req.GetCapacity(),
		Engine:   req.GetEngine(),
	}
	// MarkAgentOnline bumps agents.session_generation atomically
	// and returns the new value. We pass it INTO CreateSession so
	// the field is initialised inside the struct literal before the
	// session gets published in latestByAg — a prior version
	// assigned `sess.Generation = generation` AFTER CreateSession
	// returned, which (a) data-raced with the reaper's read in
	// FenceStaleSession and (b) opened a window where the published
	// session carried generation=0 even though the DB had already
	// bumped, letting an old reaper observation with observed=0
	// match-and-revoke the freshly-online successor.
	//
	// Why this isn't a session UUID stored in the DB: session ids
	// are bearer credentials accepted by Connect's auth. A read-only
	// DB leak (backup, log, snapshot) of a session UUID would let
	// the leak holder impersonate live sessions. A monotonic int
	// carries the "is this defer's epoch still current?" signal
	// with zero auth power.
	generation, err := a.store.MarkAgentOnline(ctx, agent.ID, upd)
	if err != nil {
		a.log.Error("agent register: update failed", "agent_id", req.GetAgentId(), "err", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	sess := a.sessions.CreateSession(agent.ID, req.GetTags(), req.GetCapacity(), generation,
		CreateSessionOpts{Engine: req.GetEngine()})

	// Now that the new session exists, fire the deferred wake-ups.
	// One NOTIFY per distinct run id — the scheduler dedups at its
	// LISTEN side anyway, but coalescing here keeps the channel
	// quieter on agents that had many concurrent jobs in flight.
	for _, runID := range notifyRunIDs {
		if err := a.store.NotifyRunQueued(ctx, runID); err != nil {
			a.log.Warn("agent register: fence notify failed",
				"agent_uuid", agent.ID, "run_id", runID, "err", err)
		}
	}
	a.log.Info("agent registered",
		"agent_id", req.GetAgentId(),
		"agent_uuid", agent.ID,
		"version", req.GetVersion(),
		"tags", req.GetTags(),
		"capacity", req.GetCapacity(),
		"session", sess.ID,
	)

	return &gocdnextv1.RegisterResponse{
		SessionId:        sess.ID,
		HeartbeatSeconds: a.heartbeatSeconds,
	}, nil
}

// RequestArtifactUpload turns the agent's list of paths into signed PUT
// tickets. Flow:
//  1. Authenticate by session_id.
//  2. Validate (run_id, job_id) pair — belongs to a real run; agent who
//     owns the session also owns the job (checked via agent_id on the
//     job_run row).
//  3. Dedupe paths by canonical form (NormalizeArtifactPath — trims
//     trailing slashes) so a YAML that lists `dist` and `dist/`
//     doesn't try to plant two rows that would collide on the
//     partial unique index (migration 00035).
//  4. Insert every pending row in ONE transaction via
//     InsertPendingArtifactsBatch — partial-loop failures roll back
//     cleanly instead of leaking orphan pending rows the operator
//     would see in the UI as ghost uploads.
//  5. Sign PUT URLs OUTSIDE the transaction (no need to hold a tx
//     open across an HTTP round-trip to S3/GCS). If signing fails,
//     the pending rows for the batch leak until the sweeper's
//     pending-TTL branch reaps them — bounded by the same grace
//     window as the existing 'deleting' sweep.
func (a *AgentService) RequestArtifactUpload(ctx context.Context, req *gocdnextv1.RequestArtifactUploadRequest) (*gocdnextv1.RequestArtifactUploadResponse, error) {
	if a.artifactStore == nil {
		return nil, status.Error(codes.Unimplemented, "artifact backend not configured")
	}
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	if req.GetRunId() == "" || req.GetJobId() == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id and job_id are required")
	}
	if len(req.GetPaths()) == 0 {
		return &gocdnextv1.RequestArtifactUploadResponse{}, nil
	}

	sess, ok := a.sessions.Lookup(req.GetSessionId())
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid session")
	}

	runID, err := uuid.Parse(req.GetRunId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed run_id")
	}
	jobRunID, err := uuid.Parse(req.GetJobId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed job_id")
	}

	pipelineID, projectID, ownerAgentID, err := a.store.JobRunParents(ctx, jobRunID, runID)
	if errors.Is(err, store.ErrArtifactNotFound) {
		return nil, status.Error(codes.NotFound, "job/run not found")
	}
	if err != nil {
		a.log.Error("artifact upload: parents lookup failed", "err", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	// Belt-and-braces authz: the session's agent must own the job.
	// Prevents session N from requesting upload URLs for a job
	// dispatched to session M. ownerAgentID == Nil means "not yet
	// dispatched" — should not happen on this path, reject.
	if ownerAgentID == uuid.Nil || ownerAgentID != sess.AgentID {
		a.log.Warn("artifact upload: session agent does not own job",
			"session_agent", sess.AgentID, "job_agent", ownerAgentID, "job_id", jobRunID)
		return nil, status.Error(codes.PermissionDenied, "job not owned by session")
	}

	expiresAt := time.Now().Add(a.artifactDefaultRetention)

	// Dedupe paths by canonical form, preserving the FIRST occurrence's
	// declared shape so the ticket round-trips the operator's typing
	// back to the agent untouched. Two-pass loop because we want
	// `dist/` to win over a later `dist` (insertion order) AND we
	// want to refuse empty paths as a hard error rather than silently
	// dropping them on dedupe.
	seen := make(map[string]struct{}, len(req.GetPaths()))
	dedupedPaths := make([]string, 0, len(req.GetPaths()))
	for _, p := range req.GetPaths() {
		if p == "" {
			return nil, status.Error(codes.InvalidArgument, "empty path in paths[]")
		}
		canon := store.NormalizeArtifactPath(p)
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		dedupedPaths = append(dedupedPaths, p)
	}

	// Build the batch + remember storage keys so the post-tx loop
	// below can sign URLs against the same keys we just persisted.
	ins := make([]store.InsertPendingArtifact, len(dedupedPaths))
	storageKeys := make([]string, len(dedupedPaths))
	for i, p := range dedupedPaths {
		storageKeys[i] = "run/" + runID.String() + "/job/" + jobRunID.String() + "/" + uuid.NewString()
		ins[i] = store.InsertPendingArtifact{
			RunID:      runID,
			JobRunID:   jobRunID,
			PipelineID: pipelineID,
			ProjectID:  projectID,
			Path:       p,
			StorageKey: storageKeys[i],
			ExpiresAt:  &expiresAt,
		}
	}
	if _, err := a.store.InsertPendingArtifactsBatch(ctx, ins); err != nil {
		a.log.Error("artifact upload: batch insert failed", "err", err)
		return nil, status.Error(codes.Internal, "persist artifacts")
	}

	tickets := make([]*gocdnextv1.ArtifactUploadTicket, 0, len(dedupedPaths))
	for i, p := range dedupedPaths {
		signed, err := a.artifactStore.SignedPutURL(ctx, storageKeys[i], a.artifactPutURLTTL)
		if err != nil {
			a.log.Error("artifact upload: sign put failed", "path", p, "err", err)
			return nil, status.Error(codes.Internal, "sign url")
		}
		tickets = append(tickets, &gocdnextv1.ArtifactUploadTicket{
			Path:       p,
			StorageKey: storageKeys[i],
			PutUrl:     signed.URL,
			ExpiresAt:  timestamppb.New(signed.ExpiresAt),
		})
	}
	a.log.Info("artifact upload tickets issued",
		"session", sess.ID, "run_id", runID, "job_id", jobRunID,
		"requested", len(req.GetPaths()), "deduped", len(tickets))
	return &gocdnextv1.RequestArtifactUploadResponse{Tickets: tickets}, nil
}
