package grpcsrv

import (
	"context"
	"errors"
	"io"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/proto/grpcconsts"
)

const offlineFlushTimeout = 2 * time.Second

// Connect accepts the agent's bidirectional stream. It validates the session
// up-front, then loops reading events from the agent. Heartbeats are answered
// synchronously with a Pong; other event kinds are currently logged and
// acknowledged implicitly by continuing the loop (scheduler wiring comes
// in a later slice).
func (a *AgentService) Connect(stream gocdnextv1.AgentService_ConnectServer) error {
	sessionID, ok := sessionFromContext(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "missing session header")
	}
	sess, ok := a.sessions.Lookup(sessionID)
	if !ok {
		return status.Error(codes.Unauthenticated, "invalid or expired session")
	}
	agentID := sess.AgentID

	log := a.log.With("agent_uuid", agentID, "session", sessionID)
	log.Info("agent stream opened")

	// Send pump: drain scheduler-produced messages onto the gRPC stream. gRPC
	// stream.Send is safe to call concurrently with stream.Recv (different
	// directions), but NOT with itself — only this goroutine writes. The pump
	// exits when the session channel is closed (on Revoke) or the stream ends.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for msg := range sess.Out() {
			if err := stream.Send(msg); err != nil {
				log.Warn("stream send failed, dropping pump", "err", err)
				return
			}
		}
	}()

	defer func() {
		a.sessions.Revoke(sessionID)
		<-pumpDone
		offCtx, cancel := context.WithTimeout(context.Background(), offlineFlushTimeout)
		defer cancel()
		if err := a.store.MarkAgentOffline(offCtx, agentID); err != nil {
			log.Warn("agent stream close: mark offline failed", "err", err)
		}
		log.Info("agent stream closed")
	}()

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if status.Code(err) == codes.Canceled {
				return nil
			}
			log.Warn("stream recv error", "err", err)
			return err
		}

		switch kind := msg.GetKind().(type) {
		case *gocdnextv1.AgentMessage_Heartbeat:
			// Pong goes through the session queue so the send pump stays the
			// single writer on stream.Send. Dropping under queue pressure is
			// fine — agents detect liveness via stream health, not pong cadence.
			pong := &gocdnextv1.ServerMessage{
				Kind: &gocdnextv1.ServerMessage_Pong{
					Pong: &gocdnextv1.Pong{At: timestamppb.Now()},
				},
			}
			if err := a.sessions.Dispatch(agentID, pong); err != nil {
				log.Debug("pong dispatch skipped", "err", err)
			}
		case *gocdnextv1.AgentMessage_Log:
			// C5 will persist into log_lines. For now log at debug so smoke
			// output is readable but noisy runs don't flood info-level sinks.
			l := kind.Log
			log.Debug("agent log",
				"run_id", l.GetRunId(), "job_id", l.GetJobId(),
				"seq", l.GetSeq(), "stream", l.GetStream(), "text", l.GetText())
		case *gocdnextv1.AgentMessage_Progress:
			log.Debug("agent progress", "kind", kindName(msg))
		case *gocdnextv1.AgentMessage_Result:
			// C5 will flip job_runs + stage_runs + runs to terminal status.
			// Surface at info so smoke tests can observe the outcome.
			r := kind.Result
			log.Info("agent job result",
				"run_id", r.GetRunId(), "job_id", r.GetJobId(),
				"status", r.GetStatus().String(), "exit_code", r.GetExitCode(),
				"error", r.GetError())
		default:
			log.Warn("stream msg: unknown kind", "kind_type", kind)
		}
	}
}

func sessionFromContext(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get(grpcconsts.SessionHeader)
	if len(vals) == 0 || vals[0] == "" {
		return "", false
	}
	return vals[0], true
}

func kindName(m *gocdnextv1.AgentMessage) string {
	switch m.GetKind().(type) {
	case *gocdnextv1.AgentMessage_Heartbeat:
		return "heartbeat"
	case *gocdnextv1.AgentMessage_Progress:
		return "progress"
	case *gocdnextv1.AgentMessage_Log:
		return "log"
	case *gocdnextv1.AgentMessage_Result:
		return "result"
	default:
		return "unknown"
	}
}
