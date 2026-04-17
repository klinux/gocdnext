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

func (c *Client) runStream(ctx context.Context, stream gocdnextv1.AgentService_ConnectClient, hb time.Duration) error {
	// Single-writer invariant for gRPC ClientStream: the same goroutine owns
	// Send and CloseSend. sendLoop runs here in the current goroutine;
	// recvLoop runs in a goroutine and only uses Recv (safe to run
	// concurrently with Send).
	sendCtx, cancelSend := context.WithCancel(ctx)
	defer cancelSend()

	recvErrCh := make(chan error, 1)
	go func() {
		recvErrCh <- c.recvLoop(stream)
		cancelSend() // recv ended → nudge sender to stop
	}()

	sendErr := c.sendLoop(sendCtx, stream, hb)
	recvErr := <-recvErrCh

	// A real send failure wins; ctx-cancel is not an error to the caller.
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

func (c *Client) sendLoop(ctx context.Context, stream gocdnextv1.AgentService_ConnectClient, hb time.Duration) error {
	// Fire an immediate heartbeat so the server sees activity on the stream.
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
		}
	}
}

func (c *Client) recvLoop(stream gocdnextv1.AgentService_ConnectClient) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		c.handleServerMessage(msg)
	}
}

func (c *Client) sendHeartbeat(stream gocdnextv1.AgentService_ConnectClient) error {
	return stream.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Heartbeat{
			Heartbeat: &gocdnextv1.Heartbeat{At: timestamppb.Now()},
		},
	})
}

func (c *Client) handleServerMessage(msg *gocdnextv1.ServerMessage) {
	switch msg.GetKind().(type) {
	case *gocdnextv1.ServerMessage_Pong:
		c.log.Debug("pong")
	case *gocdnextv1.ServerMessage_Assign:
		// TODO(phase-1-exec): execute JobAssignment.
		c.log.Info("job assignment (not implemented yet)")
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
