package grpcsrv

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/proto/grpcconsts"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
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
			// Bump last_seen_at so the reaper can tell this agent apart from
			// zombies whose stream is still half-open.
			if err := a.store.MarkAgentSeen(stream.Context(), agentID); err != nil {
				log.Warn("heartbeat: mark seen failed", "err", err)
			}
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
			a.handleLogLine(stream.Context(), log, kind.Log)
		case *gocdnextv1.AgentMessage_Progress:
			log.Debug("agent progress", "kind", kindName(msg))
		case *gocdnextv1.AgentMessage_Result:
			a.handleJobResult(stream.Context(), log, agentID, kind.Result)
		default:
			log.Warn("stream msg: unknown kind", "kind_type", kind)
		}
	}
}

// handleLogLine persists a streamed log line. Errors are logged and swallowed:
// agents should not retry on DB hiccups because seq numbers are monotonic per
// job, and the ON CONFLICT (job_run_id, seq) dedupe makes a later ack-less
// retry safe anyway.
func (a *AgentService) handleLogLine(ctx context.Context, log logger, l *gocdnextv1.LogLine) {
	jobID, err := uuid.Parse(l.GetJobId())
	if err != nil {
		log.Warn("agent log: bad job_id", "job_id", l.GetJobId())
		return
	}
	at := time.Time{}
	if l.GetAt() != nil {
		at = l.GetAt().AsTime()
	}
	if err := a.store.InsertLogLine(ctx, store.LogLine{
		JobRunID: jobID,
		Seq:      l.GetSeq(),
		Stream:   l.GetStream(),
		At:       at,
		Text:     l.GetText(),
	}); err != nil {
		log.Warn("agent log: persist failed", "err", err, "job_id", jobID, "seq", l.GetSeq())
	}
}

// handleJobResult flips the job terminal, cascades into stage + run, releases
// the agent's session capacity, and nudges the scheduler to pick up the next
// stage when the run keeps going. All errors surface as warnings — the stream
// stays open for subsequent traffic.
func (a *AgentService) handleJobResult(ctx context.Context, log logger, agentID uuid.UUID, r *gocdnextv1.JobResult) {
	jobID, err := uuid.Parse(r.GetJobId())
	if err != nil {
		log.Warn("agent result: bad job_id", "job_id", r.GetJobId())
		return
	}

	status := mapStatus(r.GetStatus())
	if status == "" {
		log.Warn("agent result: unsupported status", "status", r.GetStatus().String())
		return
	}

	comp, ok, err := a.store.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: jobID,
		Status:   status,
		ExitCode: r.GetExitCode(),
		ErrorMsg: r.GetError(),
	})
	if err != nil {
		log.Warn("agent result: complete job", "err", err, "job_id", jobID)
		return
	}
	if !ok {
		log.Debug("agent result: job already terminal", "job_id", jobID)
		return
	}

	a.sessions.Release(agentID)

	log.Info("agent job result",
		"run_id", comp.RunID, "job_id", comp.JobRunID, "job_name", comp.JobName,
		"status", status, "exit_code", r.GetExitCode(),
		"stage_done", comp.StageCompleted, "stage_status", comp.StageStatus,
		"run_done", comp.RunCompleted, "run_status", comp.RunStatus)

	// Wake the scheduler so it dispatches the next stage without waiting for
	// the periodic tick. Harmless if the run is already terminal (the handler
	// just finds no dispatchable jobs).
	if comp.StageCompleted && !comp.RunCompleted {
		if err := a.store.NotifyRunQueued(ctx, comp.RunID); err != nil {
			log.Warn("agent result: notify run_queued", "err", err)
		}
	}

	// Fanout: if the stage passed and some downstream pipeline lists this
	// (pipeline, stage) as an upstream material, queue those runs now. Errors
	// per-downstream are already joined and surfaced below — one bad sibling
	// doesn't block the others.
	if comp.StageCompleted && comp.StageStatus == string(domain.StatusSuccess) {
		triggered, fanErr := a.store.FanoutFromStage(ctx, comp.StageRunID)
		for _, t := range triggered {
			log.Info("fanout: downstream run queued",
				"downstream_pipeline_id", t.DownstreamPipelineID,
				"downstream_run_id", t.Run.RunID,
				"counter", t.Run.Counter,
				"created", t.Created)
		}
		if fanErr != nil {
			log.Warn("fanout: partial failure", "err", fanErr, "stage_run_id", comp.StageRunID)
		}
	}
}

// mapStatus converts the proto enum reported by the agent to the text labels
// used everywhere else (domain.StatusSuccess / StatusFailed).
func mapStatus(s gocdnextv1.RunStatus) string {
	switch s {
	case gocdnextv1.RunStatus_RUN_STATUS_SUCCESS:
		return string(domain.StatusSuccess)
	case gocdnextv1.RunStatus_RUN_STATUS_FAILED:
		return string(domain.StatusFailed)
	default:
		return ""
	}
}

// logger is the subset of slog.Logger the handlers rely on. Keeping it narrow
// makes it trivial to inject a test double without dragging slog internals.
type logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
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
