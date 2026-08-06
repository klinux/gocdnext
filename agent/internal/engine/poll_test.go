package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/net/http2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/gocdnext/gocdnext/agent/internal/metrics"
)

func enhanceYourCalmStatus() error {
	return status.Error(codes.ResourceExhausted,
		"stream terminated by RST_STREAM with error code: ENHANCE_YOUR_CALM")
}

func TestClassifyTransientAPIErr(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	tests := []struct {
		name          string
		err           error
		wantKind      string
		wantOK        bool
		wantRetryFrom time.Duration
	}{
		{"nil", nil, "", false, 0},
		{"plain", errors.New("boom"), "", false, 0},
		{"context canceled", context.Canceled, "", false, 0},
		{"context deadline", context.DeadlineExceeded, "", false, 0},
		{"grpc resource exhausted (enhance_your_calm)", enhanceYourCalmStatus(), "resource_exhausted", true, 0},
		{"grpc resource exhausted wrapped", fmt.Errorf("get pod: %w", enhanceYourCalmStatus()), "resource_exhausted", true, 0},
		{"grpc unavailable", status.Error(codes.Unavailable, "transport closing"), "unavailable", true, 0},
		{"grpc deadline is NOT transient", status.Error(codes.DeadlineExceeded, "x"), "", false, 0},
		{"raw http2 stream enhance_your_calm", http2.StreamError{Code: http2.ErrCodeEnhanceYourCalm}, "enhance_your_calm", true, 0},
		{"raw http2 goaway enhance_your_calm", http2.GoAwayError{ErrCode: http2.ErrCodeEnhanceYourCalm}, "enhance_your_calm", true, 0},
		{"raw http2 other code is fatal", http2.StreamError{Code: http2.ErrCodeInternal}, "", false, 0},
		{"too many requests w/ retry-after", apierrors.NewTooManyRequests("busy", 7), "too_many_requests", true, 7 * time.Second},
		{"too many requests wrapped", fmt.Errorf("w: %w", apierrors.NewTooManyRequests("busy", 3)), "too_many_requests", true, 3 * time.Second},
		{"service unavailable", apierrors.NewServiceUnavailable("no backend"), "unavailable", true, 0},
		{"server timeout", apierrors.NewServerTimeout(pods, "get", 2), "server_timeout", true, 0},
		{"timeout", apierrors.NewTimeoutError("timed out", 1), "timeout", true, 0},
		{"not found is fatal", apierrors.NewNotFound(pods, "x"), "", false, 0},
		{"forbidden is fatal", apierrors.NewForbidden(pods, "x", nil), "", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, retryAfter, ok := classifyTransientAPIErr(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v want %v (kind=%q)", ok, tt.wantOK, kind)
			}
			if kind != tt.wantKind {
				t.Errorf("kind=%q want %q", kind, tt.wantKind)
			}
			if tt.wantRetryFrom != 0 && retryAfter != tt.wantRetryFrom {
				t.Errorf("retryAfter=%v want %v", retryAfter, tt.wantRetryFrom)
			}
		})
	}
}

func TestNextDelay(t *testing.T) {
	s := time.Second
	tests := []struct {
		name            string
		prev, base, max time.Duration
		want            time.Duration
	}{
		{"first is 2*base", 0, s, 10 * s, 2 * s},
		{"doubles", 2 * s, s, 10 * s, 4 * s},
		{"doubles again", 4 * s, s, 10 * s, 8 * s},
		{"caps at max", 8 * s, s, 10 * s, 10 * s},
		{"stays capped", 10 * s, s, 10 * s, 10 * s},
		{"zero base defaults to 1s (first 2s)", 0, 0, 10 * s, 2 * s},
		{"cap raised to 2*base", 0, 30 * s, 10 * s, 60 * s},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextDelay(tt.prev, tt.base, tt.max); got != tt.want {
				t.Errorf("nextDelay(%v,%v,%v)=%v want %v", tt.prev, tt.base, tt.max, got, tt.want)
			}
		})
	}
}

func TestEqualJitter(t *testing.T) {
	if got := equalJitter(0); got != 0 {
		t.Errorf("equalJitter(0)=%v want 0", got)
	}
	d := 10 * time.Second
	for i := 0; i < 2000; i++ {
		v := equalJitter(d)
		if v < d/2 || v > d {
			t.Fatalf("equalJitter(%v)=%v out of [d/2, d]", d, v)
		}
	}
}

// sleepRecorder is an injectable sleep seam: records the durations it was asked
// to wait, and can simulate ctx expiry after N calls — no mutable global, so
// concurrent waiters (and -race) stay honest.
type sleepRecorder struct {
	calls       []time.Duration
	cancelAfter int
}

func (r *sleepRecorder) sleep(_ context.Context, d time.Duration) error {
	r.calls = append(r.calls, d)
	if r.cancelAfter > 0 && len(r.calls) >= r.cancelAfter {
		return context.DeadlineExceeded
	}
	return nil
}

func identityJitter(d time.Duration) time.Duration { return d }

func testCfg(rec *sleepRecorder) backoffCfg {
	return backoffCfg{max: 10 * time.Second, sleep: rec.sleep, jitter: identityJitter}
}

func TestPollWithBackoffImpl_DoneImmediately(t *testing.T) {
	rec := &sleepRecorder{}
	calls := 0
	err := pollWithBackoffImpl(context.Background(), "test", time.Second, testCfg(rec),
		func(context.Context) (bool, error) { calls++; return true, nil })
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 || len(rec.calls) != 0 {
		t.Errorf("calls=%d sleeps=%v; want 1 call, 0 sleeps", calls, rec.calls)
	}
}

func TestPollWithBackoffImpl_RetriesThenSucceeds(t *testing.T) {
	rec := &sleepRecorder{}
	before := testutil.ToFloat64(metrics.K8sTransientRetries.WithLabelValues("test", "resource_exhausted"))
	calls := 0
	err := pollWithBackoffImpl(context.Background(), "test", time.Second, testCfg(rec),
		func(context.Context) (bool, error) {
			calls++
			if calls <= 2 {
				return false, enhanceYourCalmStatus()
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if calls != 3 {
		t.Errorf("calls=%d want 3", calls)
	}
	// Nominal exponential progression starting at 2*base (jitter is identity).
	want := []time.Duration{2 * time.Second, 4 * time.Second}
	if len(rec.calls) != 2 || rec.calls[0] != want[0] || rec.calls[1] != want[1] {
		t.Errorf("sleeps=%v want %v", rec.calls, want)
	}
	if got := testutil.ToFloat64(metrics.K8sTransientRetries.WithLabelValues("test", "resource_exhausted")) - before; got != 2 {
		t.Errorf("metric delta=%v want 2", got)
	}
}

func TestPollWithBackoffImpl_FatalAbortsImmediately(t *testing.T) {
	rec := &sleepRecorder{}
	calls := 0
	fatal := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", nil)
	err := pollWithBackoffImpl(context.Background(), "test", time.Second, testCfg(rec),
		func(context.Context) (bool, error) { calls++; return false, fatal })
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err=%v want Forbidden", err)
	}
	if calls != 1 || len(rec.calls) != 0 {
		t.Errorf("calls=%d sleeps=%v; want 1 call, 0 sleeps", calls, rec.calls)
	}
}

func TestPollWithBackoffImpl_HonorsRetryAfter(t *testing.T) {
	rec := &sleepRecorder{}
	calls := 0
	err := pollWithBackoffImpl(context.Background(), "test", time.Second, testCfg(rec),
		func(context.Context) (bool, error) {
			calls++
			if calls == 1 {
				return false, apierrors.NewTooManyRequests("busy", 5) // Retry-After 5s
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// Retry-After (5s) is a FLOOR plus bounded dispersion (jitter(base)=1s under
	// identity) => 6s — never the exact 5s, so the fleet doesn't wake in lockstep.
	if len(rec.calls) != 1 || rec.calls[0] != 6*time.Second {
		t.Errorf("sleeps=%v want [6s] (Retry-After floor + dispersion)", rec.calls)
	}
}

func TestPollWithBackoffImpl_NoRetryCountWhenCtxExpiresInBackoff(t *testing.T) {
	before := testutil.ToFloat64(metrics.K8sTransientRetries.WithLabelValues("test2", "resource_exhausted"))
	rec := &sleepRecorder{cancelAfter: 1} // the backoff sleep itself hits ctx expiry
	calls := 0
	err := pollWithBackoffImpl(context.Background(), "test2", time.Second, testCfg(rec),
		func(context.Context) (bool, error) { calls++; return false, enhanceYourCalmStatus() })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v want DeadlineExceeded", err)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1 (ctx expired before any retry)", calls)
	}
	if got := testutil.ToFloat64(metrics.K8sTransientRetries.WithLabelValues("test2", "resource_exhausted")) - before; got != 0 {
		t.Errorf("metric delta=%v want 0 (no retry actually happened)", got)
	}
}

func TestPollWithBackoffImpl_ContextExpiryPropagates(t *testing.T) {
	rec := &sleepRecorder{cancelAfter: 2}
	calls := 0
	err := pollWithBackoffImpl(context.Background(), "test", time.Second, testCfg(rec),
		func(context.Context) (bool, error) { calls++; return false, nil }) // never done
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v want DeadlineExceeded", err)
	}
	if calls != 2 {
		t.Errorf("calls=%d want 2", calls)
	}
}
