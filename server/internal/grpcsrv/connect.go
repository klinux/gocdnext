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
	"github.com/gocdnext/gocdnext/server/internal/logstream"
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
		case *gocdnextv1.AgentMessage_TestResults:
			a.handleTestResultBatch(stream.Context(), log, kind.TestResults)
		default:
			log.Warn("stream msg: unknown kind", "kind_type", kind)
		}
	}
}

// handleLogLine persists a streamed log line. Errors are logged and swallowed:
// agents should not retry on DB hiccups because seq numbers are monotonic per
// job, and the ON CONFLICT (job_run_id, seq) dedupe makes a later ack-less
// retry safe anyway. On success we fan the event out to the in-process log
// broker (if configured) so the SSE handler can push it live without polling.
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
		return
	}
	a.publishLogLine(ctx, log, jobID, logstream.Event{
		JobRunID: jobID,
		Seq:      l.GetSeq(),
		Stream:   l.GetStream(),
		At:       at,
		Text:     l.GetText(),
	})
}

// publishLogLine fans a persisted line out to the in-process log broker.
// No-op when WithLogBroker wasn't called. The jobID→runID map is memoised
// on the service; a cold miss is one SELECT, warm is a sync.Map load.
// A missing row (ErrJobRunNotFound) means the job was swept before the
// agent's in-flight line landed — we silently drop the publish.
func (a *AgentService) publishLogLine(ctx context.Context, log logger, jobID uuid.UUID, ev logstream.Event) {
	if a.logBroker == nil {
		return
	}
	runID, err := a.runIDForJob(ctx, jobID)
	if err != nil {
		if !errors.Is(err, store.ErrJobRunNotFound) {
			log.Warn("agent log: run lookup failed", "err", err, "job_id", jobID)
		}
		return
	}
	ev.RunID = runID
	a.logBroker.Publish(ev)
}

// runIDForJob memoises jobID → runID. sync.Map's LoadOrStore keeps the
// lookup lock-free on the warm path; on a cold miss we take the DB hit
// once and cache forever (job ids never get reassigned).
func (a *AgentService) runIDForJob(ctx context.Context, jobID uuid.UUID) (uuid.UUID, error) {
	if v, ok := a.jobRunIDCache.Load(jobID); ok {
		return v.(uuid.UUID), nil
	}
	runID, err := a.store.RunIDForJobRun(ctx, jobID)
	if err != nil {
		return uuid.Nil, err
	}
	a.jobRunIDCache.Store(jobID, runID)
	return runID, nil
}

// maxTestFieldBytes caps the size of free-text fields we ingest
// from a JUnit report. A pathologically noisy test (huge stderr
// dumps) shouldn't make the wire payload — or the stored row —
// unbounded. Truncation is silent; the UI renders what landed.
const maxTestFieldBytes = 64 << 10 // 64 KiB per field

// handleTestResultBatch persists every test case in `batch`
// under its owning job_run. Errors are logged and swallowed:
// tests are a nice-to-have layer on top of the run result, a
// DB hiccup shouldn't fail the agent's stream or block the
// JobResult that comes right after.
func (a *AgentService) handleTestResultBatch(ctx context.Context, log logger, batch *gocdnextv1.TestResultBatch) {
	jobID, err := uuid.Parse(batch.GetJobId())
	if err != nil {
		log.Warn("agent test results: bad job_id", "job_id", batch.GetJobId())
		return
	}
	in := make([]store.TestResultIn, 0, len(batch.GetResults()))
	for _, r := range batch.GetResults() {
		in = append(in, store.TestResultIn{
			Suite:          r.GetSuite(),
			Classname:      r.GetClassname(),
			Name:           r.GetName(),
			Status:         store.TestResultStatus(r.GetStatus()),
			DurationMillis: r.GetDurationMillis(),
			FailureType:    clampBytes(r.GetFailureType(), maxTestFieldBytes),
			FailureMessage: clampBytes(r.GetFailureMessage(), maxTestFieldBytes),
			FailureDetail:  clampBytes(r.GetFailureDetail(), maxTestFieldBytes),
			SystemOut:      clampBytes(r.GetSystemOut(), maxTestFieldBytes),
			SystemErr:      clampBytes(r.GetSystemErr(), maxTestFieldBytes),
		})
	}
	if err := a.store.WriteTestResults(ctx, jobID, in); err != nil {
		log.Warn("agent test results: persist failed", "err", err, "job_id", jobID, "count", len(in))
		return
	}
	log.Info("agent test results persisted", "job_id", jobID, "count", len(in))
}

func clampBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
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

	// Confirm artefact uploads BEFORE marking the job done — if an agent
	// reports success but the object never made it to storage, we'd
	// rather have the job fail than let downstream jobs depend on a
	// phantom row. `confirmArtifacts` returns an error message when
	// something's off; we override the reported status in that case.
	artifactErr := a.confirmArtifacts(ctx, log, r.GetArtifacts())
	if artifactErr != "" && status == string(domain.StatusSuccess) {
		status = string(domain.StatusFailed)
		if r.GetError() == "" {
			r.Error = "artifact reconciliation: " + artifactErr
		}
		log.Warn("agent result: downgraded to failed due to artifact mismatch",
			"job_id", jobID, "detail", artifactErr)
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

	if comp.RunCompleted {
		a.checksReporter.ReportRunCompleted(ctx, comp.RunID, comp.RunStatus)
	}

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

// confirmArtifacts walks the ArtifactRef list the agent reported, HEADs
// each object in the configured backend, and flips matching DB rows
// from pending to ready. Returns an empty string on full success; a
// human-readable message when something's off (HEAD mismatch, missing
// row, short read). Missing backend = no-op so jobs without artefacts
// still succeed on an unconfigured server.
//
// Size mismatch policy: the agent's reported size is authoritative for
// the DB row, but Head() must return something non-zero for the object
// to count as ready. A Head() that succeeds but reports 0 bytes on a
// > 0 agent report is a mismatch we reject.
func (a *AgentService) confirmArtifacts(ctx context.Context, log logger, refs []*gocdnextv1.ArtifactRef) string {
	if len(refs) == 0 {
		return ""
	}
	if a.artifactStore == nil {
		// Agent shouldn't report artefacts if the server has no
		// backend, but don't fail the job over this — log and carry on.
		log.Warn("artifact confirm: refs reported but backend not configured", "count", len(refs))
		return ""
	}
	var bad []string
	for _, ref := range refs {
		key := ref.GetStorageKey()
		if key == "" {
			bad = append(bad, ref.GetPath()+" (empty storage_key)")
			continue
		}
		size, err := a.artifactStore.Head(ctx, key)
		if err != nil {
			bad = append(bad, ref.GetPath()+" (head: "+err.Error()+")")
			continue
		}
		if size == 0 && ref.GetSize() > 0 {
			bad = append(bad, ref.GetPath()+" (empty object)")
			continue
		}
		updated, err := a.store.MarkArtifactReady(ctx, key, ref.GetSize(), ref.GetContentSha256())
		if err != nil {
			bad = append(bad, ref.GetPath()+" (mark ready: "+err.Error()+")")
			continue
		}
		if !updated {
			log.Debug("artifact confirm: row not pending",
				"storage_key", key, "path", ref.GetPath())
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return "failed artifacts: " + joinCSV(bad)
}

func joinCSV(xs []string) string {
	out := ""
	for i, s := range xs {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
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
