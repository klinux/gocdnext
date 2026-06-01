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
	"github.com/gocdnext/gocdnext/server/internal/metrics"
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

	// One batcher per stream. Push from handleLogLine, drain on
	// stream close. Lifecycle is tied to Connect's defer ladder so
	// a final flush always runs even on irregular exits.
	// The receive-side (handleLogLine) captures the per-job attempt
	// snapshot from sess.LookupAssignment at Push time and tags each
	// line with it. The batcher groups by (jobID, attempt) at flush
	// and the snapshot-CAS log write decides per-group whether the
	// row still belongs to us. Doing the lookup at receive (not
	// flush) keeps the tail intact for fast-finishing jobs whose
	// JobResult triggers ClearAssignment between push and flush.
	batcher := newLogBatcher(a.store, log, agentID)
	batcher.Start(stream.Context())
	defer batcher.Stop()

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
		// Session-aware offline marking: if a successor agent
		// process already Registered (the supersededByRegister
		// flag is set by RevokeForAgent / CreateSession's internal
		// revoke), or if a different session has taken the
		// latestByAg slot, our defer must NOT clobber the agents
		// row back to 'offline'. The reaper treats offline as
		// always-stale regardless of last_seen_at, so a stray
		// offline mark from a superseded stream's defer would
		// trigger reaper reclaims on the new session's healthy
		// jobs.
		//
		// Two-flag check: supersededByRegister covers the window
		// BEFORE CreateSession publishes the new session in
		// latestByAg (where IsAgentSuperseded still returns false);
		// IsAgentSuperseded covers the post-publish state. Together
		// they close both halves of the race.
		if sess.supersededByRegister.Load() || a.sessions.IsAgentSuperseded(agentID, sessionID) {
			// Drop pending log batcher contents too: lines that
			// landed in the buffer BEFORE revoke would otherwise
			// flush on Stop and pollute the new attempt's log_lines
			// (or win the ON CONFLICT race and silently drop the
			// new attempt's legitimate lines). Discard MUST happen
			// before batcher.Stop runs its drain.
			batcher.Discard()
			log.Info("agent stream closed (superseded — leaving agents.status alone)")
			return
		}
		offCtx, cancel := context.WithTimeout(context.Background(), offlineFlushTimeout)
		defer cancel()
		// Pass THIS session's generation so the SQL CAS no-ops when
		// a successor Register has since bumped the counter. Belt
		// over the supersededByRegister suspender — handles the
		// race where this defer's Revoke() ran before any successor
		// register could find a session to flag.
		if err := a.store.MarkAgentOffline(offCtx, agentID, sess.Generation()); err != nil {
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
			// Revoked-session drop: log lines from a revoked stream
			// could re-populate rows the reaper just DELETE'd as part
			// of a retry/reclaim (see store.DeleteLogLinesByJob in the
			// reclaim path). Without this guard, an in-flight log
			// batch from the old process can land AFTER the row was
			// cleared and mix with the new attempt's output. The
			// new-attempt run picks up its own logs cleanly without
			// the stale ones interleaving.
			if sess.revoked.Load() {
				continue
			}
			a.handleLogLine(stream.Context(), log, sess, batcher, kind.Log)
		case *gocdnextv1.AgentMessage_Progress:
			log.Debug("agent progress", "kind", kindName(msg))
		case *gocdnextv1.AgentMessage_Result:
			a.handleJobResult(stream.Context(), log, sess, kind.Result)
		case *gocdnextv1.AgentMessage_TestResults:
			// Same drop policy. WriteTestResults wipes-and-reinserts
			// every test row for the job_run_id (see store.WriteTestResults),
			// so a stale revoked-session batch would CLOBBER the new
			// attempt's actual results with whatever the dying agent
			// shipped. Strictly worse than just losing the late batch.
			if sess.revoked.Load() {
				continue
			}
			a.handleTestResultBatch(stream.Context(), log, sess, kind.TestResults)
		default:
			log.Warn("stream msg: unknown kind", "kind_type", kind)
		}
	}
}

// handleLogLine routes a streamed log line into the batcher (for DB
// persistence) and publishes it synchronously to the SSE broker so
// live tailers see it without waiting for the next flush. The
// batcher dedupes via ON CONFLICT (job_run_id, seq, at), so an agent
// retransmit is harmless.
//
// Assignment-gated at receive time: sess.LookupAssignment must
// return an attempt for jobID before we either buffer the line OR
// publish it via SSE. Doing the lookup HERE (not at flush) closes
// two windows:
//
//  1. Fast-job tail loss — agent emits log, then JobResult; the
//     result handler completes the job and calls ClearAssignment;
//     when the 200ms ticker fires the assignment would be gone and
//     a flush-time lookup would drop every buffered line. Captured
//     attempt + per-(jobID, attempt) snapshot-CAS at the DB layer
//     keeps the tail intact while still rejecting genuinely stale
//     writes from a reclaimed row.
//
//  2. SSE leakage of stale streams — without this gate, a revoked-
//     but-still-draining session could publish lines to live tail
//     subscribers even though the DB write would later be dropped.
//     The receive-time check closes the BIG window (stale session
//     pushing logs after ClearAssignment / revoke). A small window
//     remains: after we capture the attempt here and publish to SSE,
//     a reclaim/rerun can still flip the row's snapshot before the
//     batcher flushes; the DB drops via ErrSnapshotStale but the
//     tail subscriber already saw the line. Closing this completely
//     would mean publishing only after the DB CAS (≥200ms latency
//     floor) or tagging events with (attempt, generation) for
//     downstream filtering — deliberately deferred.
//
// Trade-off: the SSE event lands up to flushEvery (~200ms) before
// the row is durable. A page reload in that window may briefly
// fetch a tail without the freshly-emitted line, but the live SSE
// stream catches up immediately after — the user-visible latency
// floor stays at network RTT, not DB commit.
func (a *AgentService) handleLogLine(ctx context.Context, log logger, sess *Session, batcher *logBatcher, l *gocdnextv1.LogLine) {
	jobID, err := uuid.Parse(l.GetJobId())
	if err != nil {
		log.Warn("agent log: bad job_id", "job_id", l.GetJobId())
		return
	}
	// Receive-time snapshot capture. Missing entry = the session
	// has no right to ship logs for this job (it was never assigned,
	// the terminal result already cleared it, OR a reclaim already
	// transferred ownership to a successor session). Drop silently
	// at debug so a noisy stale-agent stream doesn't drown the logs
	// — the recv-loop's revoked check upstream already warns on the
	// gross cases.
	attempt, ok := sess.LookupAssignment(jobID)
	if !ok {
		log.Debug("agent log: dropped — session has no assignment for this job",
			"session", sess.ID, "agent_uuid", sess.AgentID, "job_id", jobID)
		return
	}
	at := time.Time{}
	if l.GetAt() != nil {
		at = l.GetAt().AsTime()
	}
	batcher.Push(store.LogLine{
		JobRunID: jobID,
		Seq:      l.GetSeq(),
		Stream:   l.GetStream(),
		At:       at,
		Text:     l.GetText(),
	}, attempt)
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
//
// Assignment-gated: WriteTestResults is delete-and-reinsert per
// job_run_id. A stale session (revoked-but-still-draining, OR a
// reaper-requeued-then-redispatched scenario where this session
// happens to outlive the redispatch) writing through this handler
// would clobber the new attempt's results with the old payload.
// Looking up the per-session (job, attempt) snapshot before the
// store call drops batches that the session doesn't legitimately
// own.
func (a *AgentService) handleTestResultBatch(ctx context.Context, log logger, sess *Session, batch *gocdnextv1.TestResultBatch) {
	if sess.revoked.Load() {
		log.Warn("agent test results: dropped — session revoked",
			"session", sess.ID, "agent_uuid", sess.AgentID, "job_id", batch.GetJobId())
		return
	}
	jobID, err := uuid.Parse(batch.GetJobId())
	if err != nil {
		log.Warn("agent test results: bad job_id", "job_id", batch.GetJobId())
		return
	}
	expectedAttempt, ok := sess.LookupAssignment(jobID)
	if !ok {
		log.Warn("agent test results: dropped — session has no assignment for this job",
			"session", sess.ID, "agent_uuid", sess.AgentID, "job_id", jobID)
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
	// Snapshot-CAS write. If the row was reclaimed/redispatched
	// between LookupAssignment above and the tx below, the store
	// returns ErrSnapshotStale and we drop the batch — better to
	// lose results than to clobber the new attempt's actual ones
	// via the delete+insert pattern inside WriteTestResults.
	if err := a.store.WriteTestResults(ctx, jobID, sess.AgentID, expectedAttempt, in); err != nil {
		if errors.Is(err, store.ErrSnapshotStale) {
			log.Warn("agent test results: dropped — snapshot stale (row reclaimed/redispatched)",
				"session", sess.ID, "agent_uuid", sess.AgentID, "job_id", jobID)
			return
		}
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
//
// The whole-Session pointer (rather than just an agentID) is load-bearing:
// when a successor agent process registers, the prior session is Revoked
// but its Connect handler may still be draining inbound messages from a
// half-open stream until stream.Recv errors. A late JobResult from that
// revoked path arriving here must NOT be allowed to complete a job that
// the register-fence already reclaimed, otherwise we'd mark the new
// attempt success/failed using the old process's exit code.
//
// Two layers guard against that:
//  1. sess.revoked check below — drops the message before any DB write.
//  2. CompleteJob's predicate now validates the row's agent_id against
//     the expected agent (the calling session's), so even if the
//     revoked check is bypassed (e.g. a stale message hits at the
//     exact tick a Revoke completes), the SQL won't match a reclaimed
//     (agent_id=NULL) or redispatched (different agent) row.
func (a *AgentService) handleJobResult(ctx context.Context, log logger, sess *Session, r *gocdnextv1.JobResult) {
	if sess.revoked.Load() {
		log.Warn("agent result: dropped — session revoked",
			"session", sess.ID, "agent_uuid", sess.AgentID, "job_id", r.GetJobId())
		return
	}
	agentID := sess.AgentID
	jobID, err := uuid.Parse(r.GetJobId())
	if err != nil {
		log.Warn("agent result: bad job_id", "job_id", r.GetJobId())
		return
	}

	// Per-session assignment snapshot. The scheduler stamped
	// (jobID → attempt) when it dispatched this work TO THIS session;
	// a stale revoked-session result for a job the fence redispatched
	// (same agent_id, higher attempt on a NEW session) won't have an
	// entry here at all OR will have the OLD attempt — both cases
	// fail the SQL CAS below and the row stays safe.
	//
	// Missing entry = session never owned this job. Don't act on a
	// result the session has no right to send (an agent shouldn't be
	// reporting on a job the server never assigned to it; defending
	// here means an over-eager / replayed agent client can't be used
	// to inject completions for jobs it didn't run).
	expectedAttempt, hasAssignment := sess.LookupAssignment(jobID)
	if !hasAssignment {
		log.Warn("agent result: dropped — session has no assignment for this job",
			"session", sess.ID, "agent_uuid", agentID, "job_id", jobID)
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

	// Re-check revocation just before the DB write. confirmArtifacts can
	// take seconds (HEADs against S3), and the TOCTOU window between
	// the entry check and CompleteJob is exactly when a successor
	// Register can fence + redispatch the same job. If we got revoked
	// while reconciling, the result's snapshot is by definition stale.
	if sess.revoked.Load() {
		log.Warn("agent result: dropped post-reconcile — session revoked during handling",
			"session", sess.ID, "agent_uuid", agentID, "job_id", jobID)
		return
	}

	comp, ok, err := a.store.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        jobID,
		Status:          status,
		ExitCode:        r.GetExitCode(),
		ErrorMsg:        r.GetError(),
		ExpectedAgentID: agentID,
		ExpectedAttempt: expectedAttempt,
	})
	if err != nil {
		log.Warn("agent result: complete job", "err", err, "job_id", jobID)
		return
	}
	if !ok {
		log.Debug("agent result: job already terminal or snapshot stale",
			"job_id", jobID, "session_agent", agentID,
			"expected_attempt", expectedAttempt)
		return
	}

	// Job is terminal. Drop the per-session assignment entry so the
	// session's sync.Map stays bounded over its lifetime (one entry
	// per concurrently-running job, freed on terminal result).
	sess.ClearAssignment(jobID)
	// Decrement THIS session's running counter directly. The old
	// path went through a.sessions.Release(agentID) which looked
	// up the CURRENT session by agent — broken under a successor-
	// register race: if a new session swapped in between
	// CompleteJob's return and Release's lookup, the new session
	// would get decremented to -1 and admit one extra dispatch
	// beyond its real capacity. Going via sess directly pins the
	// decrement to the session that actually accepted the
	// assignment.
	sess.DecRunning()

	// Metrics: pair the scheduler's dispatch-time JobsRunning.Inc
	// with a Dec here so a healthy run round-trips the gauge to
	// zero. Duration histogram observes when both timestamps are
	// present (both should be by this point — `started_at` is
	// set on dispatch, `finished_at` by CompleteJobRun).
	metrics.JobsRunning.Dec()
	if comp.StartedAt != nil && comp.FinishedAt != nil {
		duration := comp.FinishedAt.Sub(*comp.StartedAt).Seconds()
		if duration >= 0 {
			metrics.JobDurationSeconds.
				WithLabelValues(metrics.JobStatusLabel(status)).
				Observe(duration)
		}
	}

	log.Info("agent job result",
		"run_id", comp.RunID, "job_id", comp.JobRunID, "job_name", comp.JobName,
		"status", status, "exit_code", r.GetExitCode(),
		"stage_done", comp.StageCompleted, "stage_status", comp.StageStatus,
		"run_done", comp.RunCompleted, "run_status", comp.RunStatus)

	// Cold-archive enqueue. The archiver runs async — the worker
	// pool will read the job's log_lines, gzip + upload, then drop
	// the rows. Per-project override is resolved live so an admin
	// toggle takes effect on the next terminating job without a
	// service restart.
	a.maybeEnqueueArchive(ctx, log, comp.JobRunID)

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

// maybeEnqueueArchive folds the global policy with the per-project
// flag and submits the job to the archiver when the result is true.
// Always nil-safe — when WithLogArchiver wasn't called the function
// short-circuits before any DB lookup.
func (a *AgentService) maybeEnqueueArchive(ctx context.Context, log logger, jobRunID uuid.UUID) {
	if a.logArchiver == nil {
		return
	}
	flag, err := a.store.GetProjectLogArchiveFlagForJob(ctx, jobRunID)
	if err != nil {
		log.Warn("logarchive: project flag lookup failed; falling back to global",
			"job_run_id", jobRunID, "err", err)
		// Continue with flag=nil so the resolver uses the global
		// policy alone — better to archive than to silently skip.
	}
	if domain.EffectiveLogArchive(a.logArchivePolicy, flag, true) {
		a.logArchiver.Submit(jobRunID)
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
