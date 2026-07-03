// Package rpc is the agent-side gRPC client: dial the server, Register, then
// keep a Connect stream alive with periodic heartbeats. Job execution plugs
// into the ServerMessage handler in a later slice.
package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/proto/grpcconsts"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/runner"
)

// Config holds what the agent needs to talk to the server. Everything except
// ServerAddr / AgentID / Token has sensible defaults.
type Config struct {
	ServerAddr string
	AgentID    string
	Token      string

	Version  string
	Tags     []string
	Capacity int32

	// Heartbeat overrides the server-suggested cadence. Zero uses the server
	// value (RegisterResponse.HeartbeatSeconds); negative falls back to 30s.
	Heartbeat time.Duration

	// WorkspaceRoot is where the runner materializes per-job workspaces.
	// Empty falls back to runner's own default (tempdir).
	WorkspaceRoot string

	// Engine is the runtime that executes each script task. Nil
	// defaults to engine.Shell inside runner.New. Set to a
	// KubernetesEngine on in-cluster deployments.
	Engine engine.Engine

	// DialOpts lets tests inject a bufconn dialer or custom credentials.
	// When nil, the client uses insecure.NewCredentials — the MVP assumes a
	// private network; TLS comes later.
	DialOpts []grpc.DialOption
}

// Client owns the long-running connection. Not safe for concurrent Run calls.
type Client struct {
	cfg Config
	log *slog.Logger
	// cleanup* is the bounded worker pool that drains incoming
	// CleanupRunServices messages. Design:
	//   - cleanupQueue: buffered channel of runIDs to clean.
	//   - cleanupPending: set of runIDs currently in the queue
	//     OR being processed by a worker, so the dispatch path
	//     can COALESCE duplicates (same runID broadcast to the
	//     same agent twice = one cleanup, idempotent at k8s
	//     anyway).
	//   - cleanupMu: guards cleanupPending.
	//
	// Why not just a semaphore? The previous semaphore design
	// dropped messages when saturated — in a burst of >N terminal
	// runs, the first N got handled and the rest were silently
	// dropped, leaving pods to leak until manual cleanup. The
	// queue+coalesce model drops only when the QUEUE is full
	// (default cap 256), and coalescing means N broadcasts to
	// the same agent for the same run count as 1 backlog slot.
	cleanupQueue   chan string
	cleanupPending map[string]struct{}
	// cleanupMaxGen carries the per-run service-generation ceiling (#97) alongside the
	// queued run id without widening the string queue: the k8s cleanup deletes only
	// pods whose generation is <= this, so a revived run's higher-generation pods
	// survive a stale cleanup. On coalesce the HIGHEST value wins (a later post-revive
	// terminal cleanup must not be masked by an earlier lower-generation supersede).
	// Guarded by cleanupMu with cleanupPending.
	cleanupMaxGen map[string]int64
	cleanupMu     sync.Mutex
	cleanupWG      sync.WaitGroup
	// drainBudget is a Background-rooted ctx with
	// cleanupShutdownDrainBudget as its deadline, installed by
	// Run()'s shutdown defer BEFORE cancelWorkers() fires. Workers
	// in the drain branch (and the queue-race recovery branch)
	// derive their per-item ctx from this so the TOTAL drain
	// wall-clock is bounded by the SHARED budget — not by N
	// independent 10s timers that could keep firing engine calls
	// after Run() already returned. Nil during steady state; the
	// drainParent helper falls back to Background so tests that
	// drive workers without Run() still work with sane defaults.
	drainBudget atomic.Pointer[context.Context]
	// cleanupAckSend is the bridge between the Run-lifetime cleanup
	// worker pool and the per-stream outbound channel. runStream
	// installs the sendOutbound callback here before recvLoop fires,
	// clears it on stream exit; cleanup workers Load atomically and
	// best-effort send a CleanupRunServicesResult after each engine
	// call. If the pointer is nil (stream disconnected between
	// dispatch and ack) the ack is silently dropped — the agent's
	// local "cleanup run services" log is the fallback record.
	cleanupAckSend atomic.Pointer[cleanupAckFunc]
}

// cleanupAckFunc is the named function type stored in
// cleanupAckSend. Named (rather than inline) so atomic.Pointer's
// generic type param resolves to a concrete type Go can take
// addresses of without the closure-stable-address rules biting.
type cleanupAckFunc func(*gocdnextv1.AgentMessage)

// newCleanupAckSender builds the non-blocking bridge between
// cleanup workers and the per-stream outbound channel. The send is
// DROP-ON-FULL by design: cleanup workers must keep dequeuing
// runIDs from the cleanup backlog even when outbound is congested,
// because the cleanup itself ALREADY happened (Engine returned
// before sendCleanupAck was called). Blocking here would freeze
// the small worker pool (4 by default) on observability bytes
// while real pod-reaper work waits in the queue — a much worse
// failure mode than silently dropping an ack the operator can
// still recover from the per-agent runCleanup log.
//
// Hoisted to a function so the non-blocking semantic is
// unit-testable without standing up a real stream. The dropped
// counter is incremented atomically when outbound rejects the
// send; logDroppedCleanupAcks reports the running total.
func newCleanupAckSender(outbound chan<- *gocdnextv1.AgentMessage, dropped *atomic.Int64) cleanupAckFunc {
	return func(msg *gocdnextv1.AgentMessage) {
		select {
		case outbound <- msg:
		default:
			dropped.Add(1)
		}
	}
}

// cleanupQueueCap is the upper bound on the cleanup backlog per
// agent. Tuned to comfortably absorb a "cancel everything" sweep
// over a moderate-sized project (each cancel produces 1 cleanup
// message PER connected k8s agent; coalescing collapses
// duplicates so only N distinct runIDs land here). The drop
// threshold above this is loud and very rare in practice.
const cleanupQueueCap = 256

// cleanupWorkers is the concurrency cap on in-flight k8s
// List/Delete calls. Bounded so a burst doesn't pressure the
// apiserver, but small enough that the worker pool itself can't
// dominate the agent's goroutine footprint.
const cleanupWorkers = 4

// New returns a client with the given config. log may be nil; a default
// logger is used.
func New(cfg Config, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		cfg:            cfg,
		log:            log,
		cleanupQueue:   make(chan string, cleanupQueueCap),
		cleanupPending: make(map[string]struct{}),
		cleanupMaxGen:  make(map[string]int64),
	}
}

// Run dials, Registers, then blocks running the heartbeat/receive loop until
// ctx is canceled or an unrecoverable error is returned. It does not retry
// on its own — the process supervisor (systemd / k8s) should restart us.
func (c *Client) Run(ctx context.Context) error {
	if err := c.cfg.validate(); err != nil {
		return err
	}

	// Cleanup worker pool lives for the lifetime of Run() — not the
	// per-stream lifetime. Today Run() is one-shot (main.go calls
	// it once; the supervisor restarts the process on failure), so
	// the in-process queue dies with Run() returning. The Run-
	// scoped pool is forward-looking: if a future patch adds
	// in-process reconnect (loop runStream on transient errors),
	// the workers, queue, and pending set are already in the right
	// scope to survive a stream restart.
	//
	// The one concrete property we get TODAY is clean shutdown
	// ordering: ctx.Done is the only shutdown trigger, and the
	// deferred drain below caps the wait so a hung
	// Engine.CleanupRunServices can't deadlock process exit.
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	for i := 0; i < cleanupWorkers; i++ {
		c.cleanupWG.Add(1)
		go c.cleanupWorkerLoop(workerCtx)
	}
	defer func() {
		// Install the shared drain-budget ctx BEFORE cancelWorkers()
		// so every worker that transitions to drain (or hits the
		// queue-race recovery branch) sees a non-nil pointer when it
		// reads drainParent. Without this, workers would each start
		// an independent 10s timer per item — and the outer select
		// below would give up at 30s while workers kept running into
		// post-Run() territory.
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), cleanupShutdownDrainBudget)
		defer cancelDrain()
		c.drainBudget.Store(&drainCtx)

		// First signal workers to stop accepting new items (via
		// ctx). Each worker drains the queue once more on the way
		// out so already-enqueued runIDs get processed — see
		// cleanupWorkerLoop. Bounded wait cap so a misbehaving k8s
		// client doesn't pin shutdown forever — note we wait on
		// drainCtx.Done() not a fresh time.After, so the wait and
		// the workers' per-item ctxs share the same deadline.
		cancelWorkers()
		done := make(chan struct{})
		go func() { c.cleanupWG.Wait(); close(done) }()
		select {
		case <-done:
		case <-drainCtx.Done():
			c.log.Warn("cleanup workers exceeded shutdown window; abandoning backlog",
				"budget", cleanupShutdownDrainBudget)
		}
		// Single-shot abandonment audit AFTER workers have stopped
		// (or the wait timed out). Doing it here rather than in
		// the per-worker drain loop means the operator sees ONE
		// Warn with the final queue+pending counts, not N=4
		// duplicate Warns racing on len(chan). Per-runID Warns
		// from the post-pop gate still fire (those carry distinct
		// run_ids and aren't duplicates).
		queued := len(c.cleanupQueue)
		c.cleanupMu.Lock()
		pending := len(c.cleanupPending)
		c.cleanupMu.Unlock()
		if queued > 0 || pending > 0 {
			c.log.Warn("cleanup backlog not fully drained at shutdown",
				"queued", queued, "pending", pending,
				"hint", "remaining run_ids may have leaked pods; check per-item post-pop Warns above for specifics")
		}
		// drainBudget intentionally left set after return: any
		// straggler goroutine that somehow outlives Wait() sees a
		// cancelled parent and exits immediately. Run() per its
		// docstring is single-call per process, so no later run
		// reuses this field.
	}()

	conn, err := c.dial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	cli := gocdnextv1.NewAgentServiceClient(conn)

	// Engine name announced on Register so the server can filter
	// run-terminal CleanupRunServices broadcasts to k8s-capable
	// agents only. Empty string when Engine is nil (boot path
	// without an engine wired); server treats unknown as
	// "broadcast-anyway" defensively.
	reg, err := cli.Register(ctx, &gocdnextv1.RegisterRequest{
		AgentId:  c.cfg.AgentID,
		Token:    c.cfg.Token,
		Version:  c.cfg.Version,
		Os:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Tags:     c.cfg.Tags,
		Capacity: c.cfg.Capacity,
		Engine:   c.engineName(),
	})
	if err != nil {
		return err
	}
	c.log.Info("registered", "session", reg.SessionId, "heartbeat_seconds", reg.HeartbeatSeconds)

	hb := c.heartbeatInterval(reg.HeartbeatSeconds)
	streamCtx := metadata.AppendToOutgoingContext(ctx, grpcconsts.SessionHeader, reg.SessionId)

	stream, err := cli.Connect(streamCtx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	uploader := NewArtifactUploader(cli, reg.SessionId, nil)
	cache := NewCacheClient(cli, reg.SessionId, nil)
	return c.runStream(ctx, stream, hb, uploader, cache)
}

// buildRunner constructs the per-session runner. Its Send callback is wired
// to the outbound channel so logs and results fan into the same single-writer
// stream pump as heartbeats.
//
// The concrete *ArtifactUploader satisfies both runner.ArtifactUploader
// (shared mode) and runner.IsolatedUploader (isolated mode), so the same
// pointer is plumbed into both slots. The concrete *CacheClient mirrors
// the pattern for the cache interfaces.
func (c *Client) buildRunner(send func(*gocdnextv1.AgentMessage), uploader *ArtifactUploader, cache *CacheClient) *runner.Runner {
	return runner.New(runner.Config{
		WorkspaceRoot:    c.cfg.WorkspaceRoot,
		Logger:           c.log,
		Send:             send,
		Uploader:         uploader,
		IsolatedUploader: uploader,
		Cache:            cache,
		IsolatedCache:    cache,
		Engine:           c.cfg.Engine,
		AgentTags:        append([]string(nil), c.cfg.Tags...),
	})
}

func (c *Client) runStream(ctx context.Context, stream gocdnextv1.AgentService_ConnectClient, hb time.Duration, uploader *ArtifactUploader, cache *CacheClient) error {
	// Single-writer invariant for gRPC ClientStream: sendLoop is the only
	// goroutine that calls stream.Send / CloseSend. Heartbeats (ticker) and
	// runner-produced messages (logs, results) both flow through `outbound`
	// so the runner can safely fan in from its own goroutine.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// outbound feeds every non-heartbeat message (logs, results,
	// progress, artefact claims) into the single-writer sendLoop.
	// Buffer is generously sized so a burst of log lines from a
	// concurrent fleet of jobs doesn't immediately stall the
	// producer.
	outbound := make(chan *gocdnextv1.AgentMessage, 4096)
	var droppedLogs atomic.Int64
	sendOutbound := func(msg *gocdnextv1.AgentMessage) {
		// Two-tier policy by message kind:
		//   - LogLine: non-blocking send. If outbound is full
		//     (server slow / network congested / many concurrent
		//     jobs spamming logs), DROP the line and increment a
		//     counter. Dropping a log line is bad for the operator
		//     UX; blocking the producer is catastrophic — the
		//     blocked engine goroutine never returns, so the
		//     JobResult never gets sent, the server never marks
		//     the job terminal, and `cancel` can't unstick the UI
		//     (the engine.streamLogs goroutine is waiting on the
		//     full channel, so even after the job's ctx is canceled
		//     the K8s engine's RunScript can't observe streamDone).
		//     The classic deadlock surfaced as: many parallel jobs,
		//     stuck "running" in the UI even after cancel.
		//   - Anything else (JobResult, ArtifactClaim, Progress,
		//     Pong, TestResults): block until delivered OR the
		//     stream is shutting down. These are low-volume and
		//     critical for correctness.
		if _, isLog := msg.Kind.(*gocdnextv1.AgentMessage_Log); isLog {
			select {
			case outbound <- msg:
			default:
				droppedLogs.Add(1)
			}
			return
		}
		select {
		case outbound <- msg:
		case <-streamCtx.Done():
		}
	}
	// Periodic warn so operators see drops in the agent log without
	// the noise of per-line warns. Ticker is cheap; the goroutine
	// exits when streamCtx cancels along with everything else.
	go c.logDroppedLines(streamCtx, &droppedLogs)
	rn := c.buildRunner(sendOutbound, uploader, cache)

	// Cleanup workers are owned by Run() (process lifetime), not by
	// this per-stream function — see Run() for the worker start +
	// bounded drain on shutdown. Reconnecting the stream no longer
	// loses the cleanup backlog because the queue and workers
	// outlive the stream.
	//
	// Bridge the per-stream outbound into the Run-lifetime cleanup
	// worker so post-cleanup acks travel up the same single-writer
	// pump. NON-BLOCKING send is load-bearing: the regular
	// sendOutbound BLOCKS for non-log messages (JobResult,
	// TestResults, …) by design — losing one of those mid-handshake
	// would corrupt the run's state. But cleanup acks are pure
	// observability and the cleanup ALREADY happened locally
	// (Engine.CleanupRunServices returned), so blocking a worker
	// here while outbound is congested would just freeze the
	// cleanup pool (4 workers × stuck-on-ack = no further pods
	// reaped) without the ack actually saving anything. Drop-on-full
	// mirrors the LogLine policy. Operators retain a per-agent
	// fallback via the runCleanup local Info/Warn log.
	//
	// The pointer is cleared on stream exit so the next reconnect
	// installs a fresh sender; acks issued between stream exit and
	// reconnect drop silently.
	var droppedCleanupAcks atomic.Int64
	ackSender := newCleanupAckSender(outbound, &droppedCleanupAcks)
	c.cleanupAckSend.Store(&ackSender)
	defer c.cleanupAckSend.Store(nil)
	go c.logDroppedCleanupAcks(streamCtx, &droppedCleanupAcks)

	recvErrCh := make(chan error, 1)
	go func() {
		recvErrCh <- c.recvLoop(streamCtx, stream, sendOutbound, rn)
		cancel()
	}()

	sendErr := c.sendLoop(streamCtx, stream, hb, outbound)
	recvErr := <-recvErrCh

	if sendErr != nil &&
		!errors.Is(sendErr, context.Canceled) &&
		!errors.Is(sendErr, context.DeadlineExceeded) {
		return sendErr
	}
	switch {
	case recvErr == nil,
		errors.Is(recvErr, io.EOF),
		errors.Is(recvErr, context.Canceled),
		errors.Is(recvErr, context.DeadlineExceeded),
		status.Code(recvErr) == codes.Canceled,
		status.Code(recvErr) == codes.DeadlineExceeded:
		return nil
	default:
		return recvErr
	}
}

func (c *Client) sendLoop(ctx context.Context, stream gocdnextv1.AgentService_ConnectClient, hb time.Duration, outbound <-chan *gocdnextv1.AgentMessage) error {
	if err := c.sendHeartbeat(stream); err != nil {
		return err
	}
	ticker := time.NewTicker(hb)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return stream.CloseSend()
		case <-ticker.C:
			if err := c.sendHeartbeat(stream); err != nil {
				return err
			}
		case msg := <-outbound:
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

func (c *Client) recvLoop(ctx context.Context, stream gocdnextv1.AgentService_ConnectClient, send func(*gocdnextv1.AgentMessage), rn *runner.Runner) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		c.handleServerMessage(ctx, msg, send, rn)
	}
}

// logDroppedLines emits a periodic WARN when sendOutbound has dropped
// log lines since the previous tick. Quiet when there's nothing to
// report so a healthy agent log stays clean. Exits when streamCtx
// cancels (stream shutting down).
func (c *Client) logDroppedLines(ctx context.Context, counter *atomic.Int64) {
	const tick = 30 * time.Second
	t := time.NewTicker(tick)
	defer t.Stop()
	var lastReported int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := counter.Load()
			if cur > lastReported {
				c.log.Warn("agent: log lines dropped (outbound full)",
					"new_drops", cur-lastReported,
					"total_drops", cur,
					"hint", "server consumer slow OR many jobs spamming logs concurrently")
				lastReported = cur
			}
		}
	}
}

// logDroppedCleanupAcks mirrors logDroppedLines: periodic delta +
// total of cleanup-ack drops so an operator can see when outbound
// congestion is silently swallowing ack messages. Pure observability
// — the cleanup itself already happened, the per-run audit trail is
// recoverable from each agent's local runCleanup log.
func (c *Client) logDroppedCleanupAcks(ctx context.Context, counter *atomic.Int64) {
	const tick = 60 * time.Second
	t := time.NewTicker(tick)
	defer t.Stop()
	var lastReported int64
	flush := func(reason string) {
		cur := counter.Load()
		if cur > lastReported {
			c.log.Warn("agent: cleanup acks dropped (outbound full)",
				"new_drops", cur-lastReported,
				"total_drops", cur,
				"reason", reason,
				"hint", "server consumer slow; per-agent runCleanup log retains the per-run record")
			lastReported = cur
		}
	}
	for {
		select {
		case <-ctx.Done():
			// Final flush before the reporter goroutine exits.
			// Drops between the last tick and stream shutdown
			// would otherwise be silently swallowed: workers
			// can still increment the closure-captured counter
			// during the brief window between cleanupAckSend
			// Store(nil) and the worker observing it via Load.
			// Better one extra Warn at shutdown than a silent
			// gap in the audit log.
			flush("stream-shutdown")
			return
		case <-t.C:
			flush("periodic")
		}
	}
}

func (c *Client) sendHeartbeat(stream gocdnextv1.AgentService_ConnectClient) error {
	return stream.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Heartbeat{
			Heartbeat: &gocdnextv1.Heartbeat{At: timestamppb.Now()},
		},
	})
}

func (c *Client) handleServerMessage(ctx context.Context, msg *gocdnextv1.ServerMessage, _ func(*gocdnextv1.AgentMessage), rn *runner.Runner) {
	switch k := msg.GetKind().(type) {
	case *gocdnextv1.ServerMessage_Pong:
		c.log.Debug("pong")
	case *gocdnextv1.ServerMessage_Assign:
		a := k.Assign
		c.log.Info("job assignment received",
			"run_id", a.GetRunId(),
			"job_id", a.GetJobId(),
			"job_name", a.GetName(),
			"image", a.GetImage(),
			"tasks", len(a.GetTasks()),
			"checkouts", len(a.GetCheckouts()))
		// Execute in its own goroutine so Recv stays responsive (cancel events,
		// next assignment). The runner publishes LogLine/JobResult through the
		// same outbound channel as heartbeats — single-writer on stream.Send.
		go rn.Execute(ctx, a)
	case *gocdnextv1.ServerMessage_Cancel:
		req := k.Cancel
		ok := rn.Cancel(req.GetJobId())
		c.log.Info("cancel requested",
			"run_id", req.GetRunId(),
			"job_id", req.GetJobId(),
			"reason", req.GetReason(),
			"matched_inflight", ok)
		// Runner.Cancel returning false is expected when the job
		// already finished or never reached this agent — we log
		// it and move on. The server relies on the eventual
		// JobResult to reconcile DB state anyway.
	case *gocdnextv1.ServerMessage_CleanupRunServices:
		// Run-terminal service teardown. The server broadcasts this
		// to a wide target set (agents of the run + every connected
		// agent), so the k8s-capable agent in the cluster gets the
		// message regardless of which agent originally created the
		// pods. Engines without k8s capability (Docker/Shell)
		// no-op.
		//
		// Runs in a goroutine so the receive loop stays responsive:
		// the cleanup's List+Delete against the k8s API can take
		// tens of seconds in the worst case, and the loop also
		// needs to deliver CancelJob frames + new JobAssignments
		// without head-of-line blocking.
		//
		// Fresh context (decoupled from the stream) with a hard cap
		// so a stream disconnect mid-cleanup doesn't kill the
		// in-flight API calls — there's no server-side retry
		// queue, so this is our only window.
		// Coalesce + enqueue. Drop is reserved for the genuine
		// queue-saturated case (256+ distinct runIDs pending);
		// duplicates of the same runID are NOT drops, they're
		// idempotent no-ops on the agent side because the k8s
		// label-selector delete is already idempotent.
		c.enqueueRunCleanup(k.CleanupRunServices.GetRunId(), k.CleanupRunServices.GetMaxGeneration())
	default:
		c.log.Warn("unknown server message kind")
	}
}

// enqueueRunCleanup is the dispatcher's entry into the worker
// pool. Two outcomes:
//
//   - Coalesce: this runID is already in the queue or being
//     processed. Skip — the existing work will complete the
//     cleanup. The k8s delete-by-label is naturally idempotent so
//     repeated identical messages from a server broadcast are
//     redundant.
//   - Enqueue: try to push the runID into the bounded channel.
//     If the channel is FULL (256+ distinct runIDs already
//     backlogged), drop with a loud warn and remove from the
//     pending set so a later message can re-enqueue. The drop
//     is the only path to a leak — bounded by the queue depth,
//     not the worker count.
//
// Workers are started in Run() (one per process lifetime), so the
// queue + workers BOTH outlive a single stream — reconnects don't
// reset the backlog or restart workers. Pending set + queue have
// aligned lifetimes via the remove-on-finish step in
// processCleanupBounded → runCleanup.
func (c *Client) enqueueRunCleanup(runID string, maxGeneration int64) {
	if runID == "" {
		return
	}
	c.cleanupMu.Lock()
	defer c.cleanupMu.Unlock()
	if _, dup := c.cleanupPending[runID]; dup {
		// Coalesced: the label-selector delete is idempotent, so one backlog slot
		// per run is enough — but keep the HIGHEST max_generation seen (#97). A
		// terminal cleanup arriving after a revive carries a higher generation than
		// an earlier supersede cleanup; masking it (delete <= lower) would leak the
		// revived generation's pods once THEY become terminal.
		if maxGeneration > c.cleanupMaxGen[runID] {
			c.cleanupMaxGen[runID] = maxGeneration
		}
		c.log.Debug("cleanup run services: coalesced duplicate", "run_id", runID)
		return
	}
	// Non-blocking channel send (default arm). The select is safe
	// under the lock because the send arm never blocks. Switched
	// to defer Unlock above so a panic mid-select can't leak the
	// mutex — load-bearing if someone ever closes cleanupQueue,
	// which would currently make the send arm panic.
	select {
	case c.cleanupQueue <- runID:
		c.cleanupPending[runID] = struct{}{}
		c.cleanupMaxGen[runID] = maxGeneration
	default:
		c.log.Warn("cleanup run services: queue saturated, dropping; pods may leak until manual cleanup",
			"run_id", runID, "queue_cap", cleanupQueueCap)
	}
}

// drainParent returns the active drain-budget ctx if one has been
// installed by Run()'s shutdown defer, otherwise context.Background().
// All shutdown-mode per-item ctxs derive from this so the GLOBAL
// drain wall-clock is bounded — see Client.drainBudget for the why.
// Tests that drive workers without Run() use SetDrainBudgetForTest
// when they want budget semantics; otherwise Background works fine
// (the unit-test engine returns instantly, no budget needed).
func (c *Client) drainParent() context.Context {
	if p := c.drainBudget.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// cleanupWorkerLoop drains cleanupQueue until ctx is canceled.
//
// Shutdown semantics:
//   - ctx is the WORKER'S parent (lives for the duration of Run).
//     During STEADY STATE it is also the parent of in-flight
//     per-cleanup contexts, so cancelWorkers() interrupts them
//     immediately rather than letting a 60s timeout run out.
//     During SHUTDOWN paths (ctx.Done arm, queue-race recovery,
//     and drainCleanupQueue) the per-cleanup parent is the
//     shared drain-budget ctx instead — see drainParent.
//   - On ctx.Done, the loop transitions to drain mode (non-
//     blocking pops) so already-enqueued runIDs at least get
//     attempted; each item derives from the drain-budget so
//     the engine has time to do real work AND the total
//     wall-clock is bounded by the SHARED budget (not N
//     independent 10s timers).
//   - Channel close (tests only) treated the same as ctx-done.
//   - After receive from the queue, the worker re-checks
//     ctx.Err() before picking a timeout. Go select randomises
//     ready-cases; without the re-check, a race where both
//     ctx.Done AND cleanupQueue are ready could land on the
//     steady-state branch with the 60s timeout and blow the
//     drain budget. The post-receive check routes through
//     processShutdownRaceItem, which uses the drain-budget
//     parent and drains the rest before returning.
func (c *Client) cleanupWorkerLoop(ctx context.Context) {
	defer c.cleanupWG.Done()
	for {
		select {
		case <-ctx.Done():
			c.drainCleanupQueue()
			return
		case runID, ok := <-c.cleanupQueue:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				c.processShutdownRaceItem(runID)
				return
			}
			// Steady-state: in-flight derives from worker ctx so
			// cancelWorkers() propagates and returns Cancelled
			// immediately on shutdown.
			c.processCleanupBounded(ctx, runID, cleanupSteadyTimeout)
		}
	}
}

// processShutdownRaceItem handles the queue-receive race where Go's
// select picks the cleanupQueue arm even though ctx.Done() is also
// ready. The worker ctx is already cancelled at this point, so the
// item must be processed against the drain-budget parent (fresh,
// shared cap) and the queue then drained before exit — identical
// to what the ctx.Done arm would have done if it had won the race.
//
// Factored out (vs. inlined in cleanupWorkerLoop) so the recovery
// path is unit-testable WITHOUT relying on Go's runtime select to
// pick the right arm — see TestCleanupWorker_ProcessShutdownRaceItem.
func (c *Client) processShutdownRaceItem(runID string) {
	c.processCleanupBounded(c.drainParent(), runID, cleanupShutdownTimeout)
	c.drainCleanupQueue()
}

// drainCleanupQueue is the shutdown-mode loop body — non-blocking
// pop, short per-item timeout, exit on empty.
//
// CRITICAL: each item gets a FRESH parent (drainParent, never the
// worker's already-cancelled ctx). The previous design derived from
// the worker ctx — drain calls then saw ctx.Done() fire immediately
// at engine entry, returned Cancelled, removed `pending`, and
// DELETED NO PODS. The "attempt cleanup of remaining backlog
// before exit" promise was violated silently.
//
// drainParent is read ONCE per call so all items in this drain
// share the same budget Done. Reading it per-item would still be
// correct (atomic), just wasteful with 256 items in the queue.
//
// Budget gating: the loop checks `parent.Err()` BEFORE every pop
// and again AFTER receiving from the queue. Once the drain budget
// has fired, dequeuing more items and calling the engine with a
// pre-cancelled ctx would log a misleading "cleanup failed"
// warning AND clear the pending entry — making a real leak look
// like a "we tried but the apiserver was slow" record. Instead
// we stop consuming so the abandoned items stay on the channel
// (queue + pending), get logged once at warn level with the
// remaining count, and the operator has an honest signal that
// cleanup did NOT complete for those runs.
func (c *Client) drainCleanupQueue() {
	parent := c.drainParent()
	for {
		// Pre-pop budget gate: items left in the queue stay there.
		// The channel + pending map are GC'd when the Client goes
		// away on process exit. NO Warn here — with 4 workers
		// concurrently in this function, an inline Warn would
		// fire up to 4× per budget-fire event with racy
		// len(channel) snapshots that won't reconcile. The
		// single-shot abandonment audit in Run()'s defer
		// (post-Wait) reports the totals once authoritatively.
		// Per-item post-pop Warns below still fire — those
		// carry distinct run_ids and aren't duplicates.
		if parent.Err() != nil {
			return
		}
		select {
		case runID, ok := <-c.cleanupQueue:
			if !ok {
				return
			}
			// Post-pop budget gate: parent could have fired in the
			// nanoseconds between the top-of-loop check and the
			// select pick. Calling the engine here would derive a
			// pre-cancelled per-call ctx — engine returns Cancelled
			// at entry, runCleanup's defer clears pending, and the
			// log claims "failed" while a real leak persists. Honest
			// alternative: drop from pending and Warn with the
			// run_id explicitly. The top-check Warn only fires when
			// there are more items left in the queue, so if THIS was
			// the last pop the abandonment would otherwise be silent.
			if perr := parent.Err(); perr != nil {
				c.cleanupMu.Lock()
				delete(c.cleanupPending, runID)
				c.cleanupMu.Unlock()
				c.log.Warn("cleanup drain budget expired post-pop; item abandoned without engine call",
					"run_id", runID, "err", perr)
				continue
			}
			c.processCleanupBounded(parent, runID, cleanupShutdownTimeout)
		default:
			return
		}
	}
}

// Timeout knobs lifted to package-level constants so tests can
// reason about expected behaviour without re-deriving the values.
const (
	cleanupSteadyTimeout       = 60 * time.Second
	cleanupShutdownTimeout     = 10 * time.Second
	cleanupShutdownDrainBudget = 30 * time.Second
)

// processCleanupBounded wraps runCleanup with a per-call timeout
// ceiling on top of `parent`. Two distinct parents reach here:
//
//   - Steady-state: parent = worker ctx, timeout = cleanupSteadyTimeout
//     (60s). cancelWorkers() propagates to interrupt in-flight engine
//     calls; the timeout is the apiserver-stuck ceiling.
//   - Shutdown (drain + race recovery): parent = drain-budget ctx
//     (Background-rooted with cleanupShutdownDrainBudget deadline),
//     timeout = cleanupShutdownTimeout (10s). Per-item ctxs SHARE
//     the same drain Done — once the global budget fires, every
//     in-flight engine call gets Cancelled and exits fast, so
//     workers can't outlive Run()'s return. (Deriving from the
//     already-cancelled worker ctx instead — the pre-round-10 bug
//     — short-circuits at engine entry and silently skips the
//     cleanup.)
func (c *Client) processCleanupBounded(parent context.Context, runID string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	c.runCleanup(ctx, runID)
}

// serviceLifecycleAck returns an emitter that translates an
// engine.ServiceLifecycleEvent into a ServiceLifecycle proto and
// pushes it through the cleanup-worker's outbound bridge (same
// drop-on-full semantics as sendCleanupAck — service lifecycle
// rows are observability, the cleanup itself already happened
// before this fires). Used by runCleanup so `stopped` events
// flow up to the server's service_runs table.
func (c *Client) serviceLifecycleAck(runID string) func(engine.ServiceLifecycleEvent) {
	return func(evt engine.ServiceLifecycleEvent) {
		send := c.cleanupAckSend.Load()
		if send == nil {
			return
		}
		(*send)(&gocdnextv1.AgentMessage{
			Kind: &gocdnextv1.AgentMessage_ServiceLifecycle{
				ServiceLifecycle: &gocdnextv1.ServiceLifecycle{
					RunId:   runID,
					Name:    evt.Name,
					Image:   evt.Image,
					PodName: evt.PodName,
					Status:  evt.Status,
					Error:   evt.Error,
					At:      timestamppb.New(time.Now().UTC()),
				},
			},
		})
	}
}

// runCleanup is the engine-level call shared between the steady-
// state worker and the shutdown drain. Both paths share the
// remove-from-pending step so coalescing stays consistent. After
// the engine call returns, it best-effort sends a
// CleanupRunServicesResult ack to the server (via the per-stream
// sendOutbound stored in cleanupAckSend) so the operator can
// audit "which agent reported what" — without it the server only
// knows it dispatched the broadcast, not whether any agent
// actually deleted anything.
func (c *Client) runCleanup(ctx context.Context, runID string) {
	if c.cfg.Engine == nil {
		// Boot path / DB-only mode: no engine, no cleanup to do.
		// The pending entry still gets removed so the next message
		// with the same runID can re-enter. No ack — no result to report.
		c.clearCleanupEntry(runID)
		return
	}
	// Loop until the generation ceiling stabilises (#97 review MED). A higher
	// max_generation can coalesce into cleanupMaxGen WHILE this engine call is running
	// (a post-revive terminal cleanup arriving during a supersede cleanup). enqueue
	// keeps the pending entry, so the raised ceiling lands in the map — but the engine
	// already got the lower value. Re-checking after each pass and re-running with the
	// raised ceiling stops the revived generation's pods from leaking. processed starts
	// below 0 (service_generation is always >= 0) so the first pass always runs.
	var processed int64 = -1
	for {
		c.cleanupMu.Lock()
		maxGeneration := c.cleanupMaxGen[runID]
		if maxGeneration <= processed {
			// No higher ceiling arrived during the last pass. Delete under the SAME
			// lock hold as the check so a concurrent enqueue can't slip a higher
			// ceiling in between (which would then be wiped unprocessed).
			delete(c.cleanupPending, runID)
			delete(c.cleanupMaxGen, runID)
			c.cleanupMu.Unlock()
			return
		}
		c.cleanupMu.Unlock()

		deleted, err := c.cfg.Engine.CleanupRunServices(ctx, runID, maxGeneration, c.serviceLifecycleAck(runID))
		if err != nil {
			c.log.Warn("cleanup run services failed", "run_id", runID, "err", err)
			c.sendCleanupAck(runID, deleted, err)
			c.clearCleanupEntry(runID)
			return
		}
		c.log.Info("cleanup run services", "run_id", runID, "deleted", deleted, "max_generation", maxGeneration)
		c.sendCleanupAck(runID, deleted, nil)
		processed = maxGeneration
	}
}

// clearCleanupEntry drops a run's pending + generation-ceiling side-map entries so a
// later CleanupRunServices for the same runID can re-enter the queue.
func (c *Client) clearCleanupEntry(runID string) {
	c.cleanupMu.Lock()
	delete(c.cleanupPending, runID)
	delete(c.cleanupMaxGen, runID)
	c.cleanupMu.Unlock()
}

// sendCleanupAck pushes a CleanupRunServicesResult up the per-stream
// outbound pump if a stream is currently connected. No-op when no
// stream is active (cleanupAckSend.Load returns nil) — by design;
// the local engine-call log above is the fallback record so the
// operator still has a trail even when the server-side ack misses.
func (c *Client) sendCleanupAck(runID string, deleted int, err error) {
	sender := c.cleanupAckSend.Load()
	if sender == nil {
		return
	}
	// Clamp deleted to int32 range. In practice this is a no-op —
	// k8s won't have 2 billion service pods per run — but a buggy
	// engine returning a 64-bit overflow value would silently
	// wrap to a negative number on the int32 cast, which the
	// server would then classify as "negative deleted, clamped to
	// 0" and Warn about. Clamping here means a runaway count is
	// reported as "very large" instead of "negative", which is a
	// less misleading signal.
	clamped := deleted
	if clamped > math.MaxInt32 {
		clamped = math.MaxInt32
	} else if clamped < 0 {
		// Defensive: Engine.CleanupRunServices contract says
		// deleted >= 0, but a panic-recovery path or bug could
		// return negative. Clamp so the server's "negative
		// deleted" Warn fires only on actual wire-level bugs.
		clamped = 0
	}
	msg := &gocdnextv1.CleanupRunServicesResult{
		RunId:   runID,
		Deleted: int32(clamped),
		Engine:  c.engineName(),
	}
	if err != nil {
		msg.ErrorMessage = err.Error()
	}
	(*sender)(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_CleanupRunServicesResult{
			CleanupRunServicesResult: msg,
		},
	})
}

// engineName returns the configured engine's Name() or "" when
// none is configured. Hoisted out so Register and sendCleanupAck
// produce the same value.
func (c *Client) engineName() string {
	if c.cfg.Engine == nil {
		return ""
	}
	return c.cfg.Engine.Name()
}

func (c *Client) heartbeatInterval(serverSeconds int32) time.Duration {
	if c.cfg.Heartbeat > 0 {
		return c.cfg.Heartbeat
	}
	if serverSeconds > 0 {
		return time.Duration(serverSeconds) * time.Second
	}
	return 30 * time.Second
}

func (c *Client) dial() (*grpc.ClientConn, error) {
	opts := c.cfg.DialOpts
	if opts == nil {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	return grpc.NewClient(c.cfg.ServerAddr, opts...)
}

func (cfg *Config) validate() error {
	if cfg.ServerAddr == "" {
		return errors.New("rpc: ServerAddr is required")
	}
	if cfg.AgentID == "" {
		return errors.New("rpc: AgentID is required")
	}
	if cfg.Token == "" {
		return errors.New("rpc: Token is required")
	}
	return nil
}
