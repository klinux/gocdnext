package grpcsrv

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
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
