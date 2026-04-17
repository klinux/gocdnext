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
	"runtime"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/proto/grpcconsts"

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

	// DialOpts lets tests inject a bufconn dialer or custom credentials.
	// When nil, the client uses insecure.NewCredentials — the MVP assumes a
	// private network; TLS comes later.
	DialOpts []grpc.DialOption
}

// Client owns the long-running connection. Not safe for concurrent Run calls.
type Client struct {
	cfg Config
	log *slog.Logger
}

// New returns a client with the given config. log may be nil; a default
// logger is used.
func New(cfg Config, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{cfg: cfg, log: log}
}

// Run dials, Registers, then blocks running the heartbeat/receive loop until
// ctx is canceled or an unrecoverable error is returned. It does not retry
// on its own — the process supervisor (systemd / k8s) should restart us.
func (c *Client) Run(ctx context.Context) error {
	if err := c.cfg.validate(); err != nil {
		return err
	}

	conn, err := c.dial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	cli := gocdnextv1.NewAgentServiceClient(conn)

	reg, err := cli.Register(ctx, &gocdnextv1.RegisterRequest{
		AgentId:  c.cfg.AgentID,
		Token:    c.cfg.Token,
		Version:  c.cfg.Version,
		Os:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Tags:     c.cfg.Tags,
		Capacity: c.cfg.Capacity,
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

	return c.runStream(ctx, stream, hb)
}

// buildRunner constructs the per-session runner. Its Send callback is wired
// to the outbound channel so logs and results fan into the same single-writer
// stream pump as heartbeats.
func (c *Client) buildRunner(send func(*gocdnextv1.AgentMessage)) *runner.Runner {
	return runner.New(runner.Config{
		WorkspaceRoot: c.cfg.WorkspaceRoot,
		Logger:        c.log,
		Send:          send,
	})
}

func (c *Client) runStream(ctx context.Context, stream gocdnextv1.AgentService_ConnectClient, hb time.Duration) error {
	// Single-writer invariant for gRPC ClientStream: sendLoop is the only
	// goroutine that calls stream.Send / CloseSend. Heartbeats (ticker) and
	// runner-produced messages (logs, results) both flow through `outbound`
	// so the runner can safely fan in from its own goroutine.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outbound := make(chan *gocdnextv1.AgentMessage, 256)
	sendOutbound := func(msg *gocdnextv1.AgentMessage) {
		select {
		case outbound <- msg:
		case <-streamCtx.Done():
		}
	}
	rn := c.buildRunner(sendOutbound)

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
		c.log.Info("cancel (not implemented yet)")
	default:
		c.log.Warn("unknown server message kind")
	}
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
