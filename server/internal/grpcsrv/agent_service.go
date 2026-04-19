package grpcsrv

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
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

	// artifactStore + artifactTTL are optional: nil store means the
	// server was started without a configured backend and artifact
	// uploads will return Unimplemented. artifactDefaultRetention is
	// what we stamp into `expires_at` when the YAML doesn't override.
	artifactStore            artifacts.Store
	artifactPutURLTTL        time.Duration
	artifactDefaultRetention time.Duration
}

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
		store:            s,
		sessions:         sessions,
		log:              log,
		heartbeatSeconds: heartbeatSeconds,
	}
}

// WithArtifactStore enables the RequestArtifactUpload RPC. Without this
// the RPC returns Unimplemented — tests that don't exercise artifacts
// don't need to wire a backend. TTLs of zero fall back to 15 min for
// the PUT URL and 30 days for the retention stamp.
func (a *AgentService) WithArtifactStore(st artifacts.Store, putURLTTL, retention time.Duration) *AgentService {
	if putURLTTL <= 0 {
		putURLTTL = 15 * time.Minute
	}
	if retention <= 0 {
		retention = 30 * 24 * time.Hour
	}
	a.artifactStore = st
	a.artifactPutURLTTL = putURLTTL
	a.artifactDefaultRetention = retention
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
		a.log.Warn("agent register: unknown agent", "agent_id", req.GetAgentId())
		return nil, status.Error(codes.NotFound, "agent not registered")
	}
	if err != nil {
		a.log.Error("agent register: lookup failed", "agent_id", req.GetAgentId(), "err", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	if !store.VerifyToken(req.GetToken(), agent.TokenHash) {
		a.log.Warn("agent register: bad token", "agent_id", req.GetAgentId())
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}

	upd := store.RegisterUpdate{
		Version:  req.GetVersion(),
		OS:       req.GetOs(),
		Arch:     req.GetArch(),
		Tags:     req.GetTags(),
		Capacity: req.GetCapacity(),
	}
	if err := a.store.MarkAgentOnline(ctx, agent.ID, upd); err != nil {
		a.log.Error("agent register: update failed", "agent_id", req.GetAgentId(), "err", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	sess := a.sessions.CreateSession(agent.ID, req.GetTags(), req.GetCapacity())
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
//  3. For each path, generate a UUID storage_key, insert a pending row,
//     sign a PUT URL.
//
// Partial failure: if path N fails, paths 0..N-1 leave pending rows in
// the DB. The sweeper will reclaim them after their TTL (or a future
// `pending_older_than(1h)` sweep) — don't unwind here; the agent can
// retry idempotently by asking for a fresh ticket per path.
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
	tickets := make([]*gocdnextv1.ArtifactUploadTicket, 0, len(req.GetPaths()))
	for _, p := range req.GetPaths() {
		if p == "" {
			return nil, status.Error(codes.InvalidArgument, "empty path in paths[]")
		}
		storageKey := "run/" + runID.String() + "/job/" + jobRunID.String() + "/" + uuid.NewString()
		if _, err := a.store.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
			RunID:      runID,
			JobRunID:   jobRunID,
			PipelineID: pipelineID,
			ProjectID:  projectID,
			Path:       p,
			StorageKey: storageKey,
			ExpiresAt:  &expiresAt,
		}); err != nil {
			a.log.Error("artifact upload: insert pending failed", "path", p, "err", err)
			return nil, status.Error(codes.Internal, "persist artifact")
		}
		signed, err := a.artifactStore.SignedPutURL(ctx, storageKey, a.artifactPutURLTTL)
		if err != nil {
			a.log.Error("artifact upload: sign put failed", "path", p, "err", err)
			return nil, status.Error(codes.Internal, "sign url")
		}
		tickets = append(tickets, &gocdnextv1.ArtifactUploadTicket{
			Path:       p,
			StorageKey: storageKey,
			PutUrl:     signed.URL,
			ExpiresAt:  timestamppb.New(signed.ExpiresAt),
		})
	}
	a.log.Info("artifact upload tickets issued",
		"session", sess.ID, "run_id", runID, "job_id", jobRunID, "count", len(tickets))
	return &gocdnextv1.RequestArtifactUploadResponse{Tickets: tickets}, nil
}
