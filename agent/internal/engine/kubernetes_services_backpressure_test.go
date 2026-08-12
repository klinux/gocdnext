package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/metrics"
)

// enhanceCalm is the shape grpc-go surfaces for an HTTP/2 RST_STREAM with
// ENHANCE_YOUR_CALM — the exact create failure the operator hit
// ("create service pod …-postgres: … ResourceExhausted … ENHANCE_YOUR_CALM").
func enhanceCalm() error {
	return status.Error(codes.ResourceExhausted,
		"stream terminated by RST_STREAM with error code: ENHANCE_YOUR_CALM")
}

// createPodsFailNThenPass returns `err` for the first n create-pods calls, then
// falls through to the tracker so the (n+1)th create actually persists the pod.
func createPodsFailNThenPass(cli *fake.Clientset, n int, err error) {
	var mu sync.Mutex
	calls := 0
	cli.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= n {
			return true, nil, err
		}
		return false, nil, nil // fall through: the tracker creates the pod
	})
}

// assignPodIPWhenExists polls until the named pod is created (racing the create
// retries), then sets its podIP — so waitForPodIP unblocks regardless of how
// many transient retries the create took.
func assignPodIPWhenExists(t *testing.T, cli *fake.Clientset, ns, name, ip string) {
	t.Helper()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for {
			pod, err := cli.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				pod.Status.PodIP = ip
				_, _ = cli.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
}

// The regression: a transient ENHANCE_YOUR_CALM on the service-pod CREATE must
// be retried (not fatal), the service still comes up, and each real retry bumps
// the metric under op="create_service_pod".
func TestEnsureServices_CreateRetriesTransientBackpressure(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: 2 * time.Second,
	})
	before := testutil.ToFloat64(
		metrics.K8sTransientRetries.WithLabelValues("create_service_pod", "resource_exhausted"))

	createPodsFailNThenPass(cli, 2, enhanceCalm())
	assignPodIPWhenExists(t, cli, "gocdnext-tests", "gocdnext-svc-runbp123abcd-g0-postgres", "10.0.0.10")

	wireup, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		"runbp123abcd", "job-1", 0, nil, nil)
	if err != nil {
		t.Fatalf("expected transient create backpressure to be retried, got %v", err)
	}
	if len(wireup.HostAliases) != 1 {
		t.Fatalf("hostAliases = %v, want 1 (service came up)", wireup.HostAliases)
	}
	if got := testutil.ToFloat64(
		metrics.K8sTransientRetries.WithLabelValues("create_service_pod", "resource_exhausted")) - before; got != 2 {
		t.Errorf("retry metric delta = %v, want 2", got)
	}
}

// A non-transient create error (Forbidden) must still fail the job immediately —
// retry is scoped to backpressure, and the error names the pod (never a value).
func TestEnsureServices_CreateFatalErrorStillFails(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: 200 * time.Millisecond,
	})
	cli.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, kerrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "svc", errors.New("nope"))
	})

	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		"runfatal1234", "job-1", 0, nil, nil)
	if err == nil {
		t.Fatal("expected Forbidden create to remain fatal")
	}
	if !strings.Contains(err.Error(), "create service pod") {
		t.Errorf("error should call out the create failure: %v", err)
	}
}
