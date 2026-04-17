package rpc_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/proto/grpcconsts"

	"github.com/gocdnext/gocdnext/agent/internal/rpc"
)

func TestClient_RegisterAndHeartbeat(t *testing.T) {
	fake := newFakeServer(t, map[string]string{"agent-1": "tok"})

	client := rpc.New(rpc.Config{
		ServerAddr: "passthrough:///bufnet",
		AgentID:    "agent-1",
		Token:      "tok",
		Version:    "test-0.0.1",
		Tags:       []string{"docker"},
		Capacity:   2,
		Heartbeat:  20 * time.Millisecond,
		DialOpts: []grpc.DialOption{
			grpc.WithContextDialer(fake.dialer()),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := client.Run(ctx)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v", err)
	}

	if got := fake.heartbeatCount(); got < 2 {
		t.Fatalf("heartbeats = %d, want >= 2", got)
	}
	if got := fake.registerCount(); got != 1 {
		t.Fatalf("registers = %d, want 1", got)
	}

	req := fake.lastRegister()
	if req.AgentId != "agent-1" || req.Token != "tok" || req.Version != "test-0.0.1" || req.Capacity != 2 {
		t.Fatalf("register req = %+v", req)
	}
}

func TestClient_UnknownAgentReturnsError(t *testing.T) {
	fake := newFakeServer(t, map[string]string{})

	client := rpc.New(rpc.Config{
		ServerAddr: "passthrough:///bufnet",
		AgentID:    "nobody",
		Token:      "t",
		Heartbeat:  20 * time.Millisecond,
		DialOpts: []grpc.DialOption{
			grpc.WithContextDialer(fake.dialer()),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := client.Run(ctx)
	if err == nil {
		t.Fatalf("expected error on unknown agent, got nil")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", code)
	}
}

// --- fake server ---

type fakeServer struct {
	gocdnextv1.UnimplementedAgentServiceServer

	tokensByName map[string]string
	lis          *bufconn.Listener
	grpcSrv      *grpc.Server

	mu        sync.Mutex
	sessions  map[string]string // sess -> agentID
	regReqs   []*gocdnextv1.RegisterRequest
	heartbeat atomic.Int64
}

func newFakeServer(t *testing.T, tokens map[string]string) *fakeServer {
	t.Helper()
	f := &fakeServer{
		tokensByName: tokens,
		sessions:     make(map[string]string),
		lis:          bufconn.Listen(1 << 20),
	}
	f.grpcSrv = grpc.NewServer()
	gocdnextv1.RegisterAgentServiceServer(f.grpcSrv, f)
	go func() { _ = f.grpcSrv.Serve(f.lis) }()
	t.Cleanup(func() {
		f.grpcSrv.GracefulStop()
		_ = f.lis.Close()
	})
	return f
}

func (f *fakeServer) dialer() func(context.Context, string) (net.Conn, error) {
	return func(_ context.Context, _ string) (net.Conn, error) {
		return f.lis.Dial()
	}
}

func (f *fakeServer) heartbeatCount() int { return int(f.heartbeat.Load()) }

func (f *fakeServer) registerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.regReqs)
}

func (f *fakeServer) lastRegister() *gocdnextv1.RegisterRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.regReqs) == 0 {
		return nil
	}
	return f.regReqs[len(f.regReqs)-1]
}

func (f *fakeServer) Register(_ context.Context, req *gocdnextv1.RegisterRequest) (*gocdnextv1.RegisterResponse, error) {
	want, ok := f.tokensByName[req.AgentId]
	if !ok {
		return nil, status.Error(codes.NotFound, "unknown agent")
	}
	if want != req.Token {
		return nil, status.Error(codes.Unauthenticated, "bad token")
	}
	sess := uuid.NewString()
	f.mu.Lock()
	f.sessions[sess] = req.AgentId
	f.regReqs = append(f.regReqs, req)
	f.mu.Unlock()
	return &gocdnextv1.RegisterResponse{SessionId: sess, HeartbeatSeconds: 1}, nil
}

func (f *fakeServer) Connect(stream gocdnextv1.AgentService_ConnectServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	vals := md.Get(grpcconsts.SessionHeader)
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "no session")
	}
	f.mu.Lock()
	_, ok := f.sessions[vals[0]]
	f.mu.Unlock()
	if !ok {
		return status.Error(codes.Unauthenticated, "invalid session")
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if _, ok := msg.Kind.(*gocdnextv1.AgentMessage_Heartbeat); ok {
			f.heartbeat.Add(1)
			if err := stream.Send(&gocdnextv1.ServerMessage{
				Kind: &gocdnextv1.ServerMessage_Pong{Pong: &gocdnextv1.Pong{At: timestamppb.Now()}},
			}); err != nil {
				return err
			}
		}
	}
}
