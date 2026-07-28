package grpcsrv_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/proto/grpcconsts"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestConnect_NoSessionHeader(t *testing.T) {
	_, client := bootServer(t)

	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_, recvErr := stream.Recv()
	if code := status.Code(recvErr); code != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", code)
	}
}

func TestConnect_InvalidSession(t *testing.T) {
	_, client := bootServer(t)

	ctx := metadata.AppendToOutgoingContext(context.Background(),
		grpcconsts.SessionHeader, "not-a-real-session")
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_, recvErr := stream.Recv()
	if code := status.Code(recvErr); code != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", code)
	}
}

func TestConnect_HeartbeatGetsPong(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-hb", store.HashToken("tok"))

	reg, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-hb",
		Token:   "tok",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, grpcconsts.SessionHeader, reg.SessionId)

	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	err = stream.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Heartbeat{
			Heartbeat: &gocdnextv1.Heartbeat{At: timestamppb.Now(), RunningJobs: 0},
		},
	})
	if err != nil {
		t.Fatalf("Send heartbeat: %v", err)
	}

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	pong, ok := msg.Kind.(*gocdnextv1.ServerMessage_Pong)
	if !ok {
		t.Fatalf("expected Pong, got %T", msg.Kind)
	}
	if pong.Pong.At == nil {
		t.Fatalf("Pong.At is nil")
	}

	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	// Server closes cleanly with io.EOF or nil on its side; client sees io.EOF.
	if _, err := stream.Recv(); err != nil && err != io.EOF {
		// any grpc-level error other than EOF is a failure (cancel is OK)
		if code := status.Code(err); code != codes.Canceled && code != codes.OK {
			t.Fatalf("final Recv err = %v", err)
		}
	}
}

func TestConnect_DrainingKeepsStreamLive(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-drain", store.HashToken("tok"))

	reg, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-drain", Token: "tok",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, grpcconsts.SessionHeader, reg.SessionId)

	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// A Draining frame is accepted (recv loop handles the new kind) and the
	// session stays LIVE — a subsequent heartbeat still gets a Pong.
	if err := stream.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Draining{Draining: &gocdnextv1.Draining{}},
	}); err != nil {
		t.Fatalf("Send draining: %v", err)
	}
	if err := stream.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Heartbeat{Heartbeat: &gocdnextv1.Heartbeat{At: timestamppb.Now()}},
	}); err != nil {
		t.Fatalf("Send heartbeat after draining: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv after draining: %v", err)
	}
	if _, ok := msg.Kind.(*gocdnextv1.ServerMessage_Pong); !ok {
		t.Fatalf("expected Pong after draining (session still live), got %T", msg.Kind)
	}
	_ = stream.CloseSend()
}

// A draining session with no in-flight jobs, once its stream closes, emits
// gocdnext_agent_drain_total{clean} (and, being co-located, the duration
// observation) from the Connect defer. The metric fires server-side after the
// recv loop returns, so we poll for it.
func TestConnect_DrainMetricCleanOnClose(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-drainmetric", store.HashToken("tok"))

	reg, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-drainmetric", Token: "tok",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, grpcconsts.SessionHeader, reg.SessionId)

	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	cleanBefore := testutil.ToFloat64(metrics.AgentDrain.WithLabelValues("clean"))

	// Drain, then a heartbeat→pong round-trip confirms the server processed the
	// Draining frame (so drainStartedAt is set) before we close.
	if err := stream.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Draining{Draining: &gocdnextv1.Draining{}},
	}); err != nil {
		t.Fatalf("Send draining: %v", err)
	}
	if err := stream.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Heartbeat{Heartbeat: &gocdnextv1.Heartbeat{At: timestamppb.Now()}},
	}); err != nil {
		t.Fatalf("Send heartbeat: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv pong: %v", err)
	}

	// Close → the server's recv loop returns → the Connect defer runs → metric.
	_ = stream.CloseSend()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	var delta float64
	for i := 0; i < 100; i++ {
		delta = testutil.ToFloat64(metrics.AgentDrain.WithLabelValues("clean")) - cleanBefore
		if delta == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if delta != 1 {
		t.Fatalf("agent_drain_total{clean} delta = %v, want 1 (no in-flight jobs → clean)", delta)
	}
}

func TestConnect_SessionRevokedOnStreamClose(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-revoke", store.HashToken("tok"))

	reg, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-revoke",
		Token:   "tok",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctx = metadata.AppendToOutgoingContext(ctx, grpcconsts.SessionHeader, reg.SessionId)
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	// send a heartbeat so the stream is definitely established on server side
	_ = stream.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Heartbeat{Heartbeat: &gocdnextv1.Heartbeat{At: timestamppb.Now()}},
	})
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("handshake Recv: %v", err)
	}
	cancel()
	// give the server a moment to notice cancel and run cleanup
	time.Sleep(100 * time.Millisecond)

	// trying to reconnect with the same session must fail
	ctx2 := metadata.AppendToOutgoingContext(context.Background(),
		grpcconsts.SessionHeader, reg.SessionId)
	stream2, err := client.Connect(ctx2)
	if err != nil {
		t.Fatalf("reopen stream: %v", err)
	}
	if _, err := stream2.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("reconnect with old session: code = %s, want Unauthenticated", status.Code(err))
	}
}
