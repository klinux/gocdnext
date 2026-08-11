package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/gocdnext/gocdnext/agent/internal/metrics"
)

// newRetryEngine builds an engine whose only load-bearing config for
// createWithBackoff is the (tiny) poll interval + startup budget. The client is
// unused: createWithBackoff drives a caller-supplied create closure, so these
// tests script the API responses directly instead of a fake clientset.
func newRetryEngine() *Kubernetes {
	return NewKubernetesWithClient(nil, KubernetesConfig{
		Namespace:      "ci",
		PollInterval:   time.Millisecond,
		StartupTimeout: 5 * time.Second,
	})
}

func retryCount(t *testing.T, op, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.K8sTransientRetries.WithLabelValues(op, reason))
}

// The exact failure the operator hit: a create returns ENHANCE_YOUR_CALM
// (grpc ResourceExhausted). It must be retried, not fatal, and each real retry
// bumps the metric.
func TestCreateWithBackoff_RetriesTransient(t *testing.T) {
	k := newRetryEngine()
	before := retryCount(t, "create_test", "resource_exhausted")

	calls := 0
	err := k.createWithBackoff(context.Background(), "create_test", func(context.Context) error {
		calls++
		if calls <= 2 {
			return enhanceYourCalm()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected transient create to be retried, got %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (2 transient + 1 success)", calls)
	}
	if got := retryCount(t, "create_test", "resource_exhausted") - before; got != 2 {
		t.Errorf("retry metric delta = %v, want 2", got)
	}
}

// A non-transient error (Forbidden) aborts on the first attempt — retry is
// scoped to backpressure only.
func TestCreateWithBackoff_FatalStopsImmediately(t *testing.T) {
	k := newRetryEngine()
	fatal := kerrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p", errors.New("nope"))

	calls := 0
	err := k.createWithBackoff(context.Background(), "create_test_fatal", func(context.Context) error {
		calls++
		return fatal
	})
	if err == nil {
		t.Fatal("expected Forbidden to remain fatal")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on fatal)", calls)
	}
}

// AlreadyExists on the FIRST attempt is a genuine name collision (unique pod
// names), so it must stay fatal — never silently adopt a stranger's pod.
func TestCreateWithBackoff_AlreadyExistsFirstAttemptFatal(t *testing.T) {
	k := newRetryEngine()
	ae := kerrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, "p")

	calls := 0
	err := k.createWithBackoff(context.Background(), "create_test_ae1", func(context.Context) error {
		calls++
		return ae
	})
	if err == nil {
		t.Fatal("expected AlreadyExists on the first attempt to remain fatal")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// AlreadyExists AFTER a transient retry means our own prior attempt committed
// before the RST_STREAM ate the response — adopt it as success (idempotent),
// otherwise a retry would convert a recoverable blip into a hard failure.
func TestCreateWithBackoff_AlreadyExistsAfterRetryAdopted(t *testing.T) {
	k := newRetryEngine()
	ae := kerrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, "p")

	calls := 0
	err := k.createWithBackoff(context.Background(), "create_test_ae2", func(context.Context) error {
		calls++
		if calls == 1 {
			return enhanceYourCalm() // committed, but response lost to backpressure
		}
		return ae // retry observes our own committed pod
	})
	if err != nil {
		t.Fatalf("expected AlreadyExists-after-retry to be adopted, got %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// A create that never reached the server (ctx already cancelled → the client
// returns the context error before the wire) must abort AND must NOT be flagged
// ambiguous: nothing could have committed.
func TestCreateWithBackoff_CleanCancelIsNotAmbiguous(t *testing.T) {
	k := newRetryEngine()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := k.createWithBackoff(ctx, "create_test_cancel", func(ctx context.Context) error {
		if ctx.Err() != nil {
			return ctx.Err() // like a real client: request never sent
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected cancelled ctx to abort")
	}
	if errors.Is(err, errCreateAmbiguous) {
		t.Errorf("a create that never reached the server must not be ambiguous: %v", err)
	}
}

// An earlier transient attempt (commit unknown) followed by a LATER external
// cancel whose create returns context.Canceled must STILL be flagged ambiguous —
// the late cancel must not erase the earlier attempt's unknown commit.
func TestCreateWithBackoff_AmbiguousWhenEarlierTransientThenCancel(t *testing.T) {
	k := NewKubernetesWithClient(nil, KubernetesConfig{
		Namespace:      "ci",
		PollInterval:   time.Millisecond,
		StartupTimeout: 5 * time.Second, // long: the CANCEL drives the exit, not a timeout
	})
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	err := k.createWithBackoff(ctx, "create_test_late_cancel", func(c context.Context) error {
		calls++
		if calls == 1 {
			return enhanceYourCalm() // transient: commit unknown
		}
		cancel()       // external cancel before the 2nd attempt resolves
		return c.Err() // a real client returns the context error
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, errCreateAmbiguous) {
		t.Errorf("earlier transient + later cancel must remain ambiguous, got %v", err)
	}
	if calls < 2 {
		t.Errorf("expected the second (cancelling) attempt to run, calls=%d", calls)
	}
}

// When the transient budget is exhausted while the LAST attempt was still
// transient, the server-side outcome is unknown → the error must carry
// errCreateAmbiguous so the caller reconciles instead of leaking.
func TestCreateWithBackoff_AmbiguousWhenTransientBudgetExhausted(t *testing.T) {
	k := NewKubernetesWithClient(nil, KubernetesConfig{
		Namespace:      "ci",
		PollInterval:   time.Millisecond,
		StartupTimeout: 20 * time.Millisecond, // small budget: exhausts mid-backoff
	})

	calls := 0
	err := k.createWithBackoff(context.Background(), "create_test_ambig", func(context.Context) error {
		calls++
		return enhanceYourCalm() // never succeeds
	})
	if err == nil {
		t.Fatal("expected an error once the transient budget is exhausted")
	}
	if !errors.Is(err, errCreateAmbiguous) {
		t.Errorf("expected errCreateAmbiguous, got %v", err)
	}
	if calls < 1 {
		t.Errorf("expected at least one create attempt, got %d", calls)
	}
}
