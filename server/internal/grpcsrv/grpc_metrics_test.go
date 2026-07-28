package grpcsrv_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/proto/grpcconsts"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestMetricsUnaryInterceptor(t *testing.T) {
	const method = "/gocdnext.v1.AgentService/UnaryTest"
	info := &grpc.UnaryServerInfo{FullMethod: method}

	// Success → handled{OK} + started + one histogram observation.
	startedBefore := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(method))
	okBefore := testutil.ToFloat64(metrics.GRPCServerHandled.WithLabelValues(method, "OK"))
	if _, err := grpcsrv.MetricsUnaryInterceptor(context.Background(), nil, info,
		func(context.Context, any) (any, error) { return "ok", nil }); err != nil {
		t.Fatalf("interceptor err: %v", err)
	}
	if d := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(method)) - startedBefore; d != 1 {
		t.Fatalf("started delta = %v, want 1", d)
	}
	if d := testutil.ToFloat64(metrics.GRPCServerHandled.WithLabelValues(method, "OK")) - okBefore; d != 1 {
		t.Fatalf("handled{OK} delta = %v, want 1", d)
	}

	// Error → the status code labels handled, NOT "OK".
	errBefore := testutil.ToFloat64(metrics.GRPCServerHandled.WithLabelValues(method, "PermissionDenied"))
	_, _ = grpcsrv.MetricsUnaryInterceptor(context.Background(), nil, info,
		func(context.Context, any) (any, error) { return nil, status.Error(codes.PermissionDenied, "nope") })
	if d := testutil.ToFloat64(metrics.GRPCServerHandled.WithLabelValues(method, "PermissionDenied")) - errBefore; d != 1 {
		t.Fatalf("handled{PermissionDenied} delta = %v, want 1", d)
	}
}

func TestMetricsStreamInterceptor_RecordsOncePerStream(t *testing.T) {
	const method = "/gocdnext.v1.AgentService/StreamTest"
	info := &grpc.StreamServerInfo{FullMethod: method, IsServerStream: true, IsClientStream: true}

	startedBefore := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(method))
	handledBefore := testutil.ToFloat64(metrics.GRPCServerHandled.WithLabelValues(method, "Canceled"))
	// The handler runs the whole "stream"; the interceptor must record exactly
	// once at open and once at close, regardless of message volume inside.
	err := grpcsrv.MetricsStreamInterceptor(nil, nil, info,
		func(any, grpc.ServerStream) error { return status.Error(codes.Canceled, "closed") })
	if code := status.Code(err); code != codes.Canceled {
		t.Fatalf("passthrough err code = %s, want Canceled", code)
	}
	if d := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(method)) - startedBefore; d != 1 {
		t.Fatalf("started delta = %v, want 1", d)
	}
	if d := testutil.ToFloat64(metrics.GRPCServerHandled.WithLabelValues(method, "Canceled")) - handledBefore; d != 1 {
		t.Fatalf("handled{Canceled} delta = %v, want 1", d)
	}
}

// InitGRPCMetrics must pre-seed a series for every AgentService method (from the
// generated ServiceDesc), so the panels aren't blank before first traffic.
func TestInitGRPCMetrics_PreseedsEveryMethod(t *testing.T) {
	grpcsrv.InitGRPCMetrics()
	present := gatheredMethodLabels(t, "gocdnext_grpc_server_started_total")
	want := []string{
		"/gocdnext.v1.AgentService/Register",
		"/gocdnext.v1.AgentService/RequestCacheGet",
		"/gocdnext.v1.AgentService/RequestCachePut",
		"/gocdnext.v1.AgentService/MarkCacheReady",
		"/gocdnext.v1.AgentService/RequestArtifactUpload",
		"/gocdnext.v1.AgentService/Connect",
	}
	for _, m := range want {
		if !present[m] {
			t.Fatalf("method %q not pre-seeded (got %v)", m, present)
		}
	}
}

// A real Register (unary) + a cache RPC (proves the unary interceptor isn't
// Register-only) + a Connect stream must all move their gRPC series.
func TestGRPCMetrics_RoundTrip(t *testing.T) {
	pool, client := bootServer(t)
	seedAgentViaSQL(t, pool, "runner-grpcm", store.HashToken("tok"))

	regMethod := "/gocdnext.v1.AgentService/Register"
	cacheMethod := "/gocdnext.v1.AgentService/RequestCacheGet"
	connMethod := "/gocdnext.v1.AgentService/Connect"

	regBefore := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(regMethod))
	cacheBefore := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(cacheMethod))
	connBefore := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(connMethod))

	reg, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{AgentId: "runner-grpcm", Token: "tok"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Cache RPC returns Unimplemented here (no artifact store) — the interceptor
	// still records it, which is the point (unary covers more than Register).
	_, _ = client.RequestCacheGet(context.Background(), &gocdnextv1.RequestCacheGetRequest{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, grpcconsts.SessionHeader, reg.SessionId)
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_ = stream.Send(&gocdnextv1.AgentMessage{Kind: &gocdnextv1.AgentMessage_Heartbeat{Heartbeat: &gocdnextv1.Heartbeat{At: timestamppb.Now()}}})
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv: %v", err)
	}

	if d := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(regMethod)) - regBefore; d < 1 {
		t.Fatalf("Register started delta = %v, want >= 1", d)
	}
	if d := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(cacheMethod)) - cacheBefore; d < 1 {
		t.Fatalf("RequestCacheGet started delta = %v, want >= 1 (unary must not be Register-only)", d)
	}
	if d := testutil.ToFloat64(metrics.GRPCServerStarted.WithLabelValues(connMethod)) - connBefore; d < 1 {
		t.Fatalf("Connect started delta = %v, want >= 1", d)
	}
}

// The stream interceptor records only at open/close — it never wraps
// RecvMsg/SendMsg, so per-message cost on the Connect firehose is zero. This
// bench pins the tiny per-STREAM overhead; a per-message regression would show
// up as a new allocation/cost here only if someone wrapped the message path.
func BenchmarkMetricsStreamInterceptor(b *testing.B) {
	info := &grpc.StreamServerInfo{FullMethod: "/gocdnext.v1.AgentService/Connect"}
	noop := func(any, grpc.ServerStream) error { return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = grpcsrv.MetricsStreamInterceptor(nil, nil, info, noop)
	}
}

func gatheredMethodLabels(t *testing.T, metricName string) map[string]bool {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "grpc_method" {
					got[l.GetValue()] = true
				}
			}
		}
	}
	return got
}
