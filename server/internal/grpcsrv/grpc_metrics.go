package grpcsrv

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
)

// gRPC server metrics (#191), hand-rolled instead of go-grpc-prometheus so the
// Connect bidi stream — the log-line firehose — pays NO per-message cost: the
// stream interceptor records once at open and once at close, and never wraps
// RecvMsg/SendMsg. Unary RPCs additionally get a handling-time histogram.

// MetricsUnaryInterceptor records started + handled{code} + a handling-time
// histogram for unary RPCs (Register + the cache/artifact calls, where a
// request/response latency is meaningful).
func MetricsUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	metrics.GRPCServerStarted.WithLabelValues(info.FullMethod).Inc()
	start := time.Now()
	resp, err := handler(ctx, req)
	metrics.GRPCServerHandling.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
	metrics.GRPCServerHandled.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
	return resp, err
}

// MetricsStreamInterceptor records started (open) + handled{code} (close) ONCE
// per stream for Connect. No per-message counter and no handling-time histogram:
// the stream is long-lived (its handling time is the whole session), and a
// per-message Inc on the log firehose is exactly the hot-path cost we avoid.
func MetricsStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	metrics.GRPCServerStarted.WithLabelValues(info.FullMethod).Inc()
	err := handler(srv, ss)
	metrics.GRPCServerHandled.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
	return err
}

// InitGRPCMetrics pre-seeds the zero series for every AgentService method so
// rate()/error-rate panels read from the first scrape (before any traffic). The
// method names derive from the generated ServiceDesc — never hand-written — so
// they match info.FullMethod exactly and can't drift when the proto changes.
// Seeds code="OK" per method (makes the per-method handled total live); other
// codes appear on first occurrence.
func InitGRPCMetrics() {
	sd := gocdnextv1.AgentService_ServiceDesc
	seed := func(fullMethod string, unary bool) {
		metrics.GRPCServerStarted.WithLabelValues(fullMethod)
		metrics.GRPCServerHandled.WithLabelValues(fullMethod, codes.OK.String())
		if unary {
			metrics.GRPCServerHandling.WithLabelValues(fullMethod)
		}
	}
	for _, m := range sd.Methods {
		seed("/"+sd.ServiceName+"/"+m.MethodName, true)
	}
	for _, st := range sd.Streams {
		seed("/"+sd.ServiceName+"/"+st.StreamName, false)
	}
}
