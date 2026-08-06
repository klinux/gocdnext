package engine

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/gocdnext/gocdnext/agent/internal/metrics"
)

// maxBackoff caps the nominal (pre-jitter) backoff between poll attempts once
// transient backpressure appears. With PollInterval=1s and StartupTimeout=5m
// this still leaves many attempts before the budget runs out. The
// backoff-driven sleep never exceeds this (equal jitter stays within [d/2, d]);
// a larger server-supplied Retry-After can, since it is honoured as a floor.
const maxBackoff = 10 * time.Second

// classifyTransientAPIErr reports whether err is a retryable backpressure /
// availability signal from the Kubernetes API (or a gRPC proxy in front of
// it), a CLOSED-set kind for the metric, and any server-suggested Retry-After.
//
// The canonical case: an HTTP/2 RST_STREAM with ENHANCE_YOUR_CALM, which
// grpc-go surfaces as codes.ResourceExhausted — the exact error that failed
// prep on the isolated job pods under apiserver flood protection.
//
// context.Canceled / DeadlineExceeded are NEVER transient (they are the
// caller's budget expiring or an explicit cancel) and are checked FIRST.
// Classification is by TYPE, never by message substring.
func classifyTransientAPIErr(err error) (kind string, retryAfter time.Duration, ok bool) {
	if err == nil {
		return "", 0, false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", 0, false
	}
	// 429 / ServerTimeout can carry a server-suggested delay; honour it.
	if secs, has := apierrors.SuggestsClientDelay(err); has {
		retryAfter = time.Duration(secs) * time.Second
	}
	switch {
	case apierrors.IsTooManyRequests(err):
		return "too_many_requests", retryAfter, true
	case apierrors.IsServiceUnavailable(err):
		return "unavailable", retryAfter, true
	case apierrors.IsServerTimeout(err):
		return "server_timeout", retryAfter, true
	case apierrors.IsTimeout(err):
		return "timeout", retryAfter, true
	}
	// gRPC status: ENHANCE_YOUR_CALM -> ResourceExhausted; transport -> Unavailable.
	if s, isStatus := status.FromError(err); isStatus {
		switch s.Code() {
		case codes.ResourceExhausted:
			return "resource_exhausted", retryAfter, true
		case codes.Unavailable:
			return "unavailable", retryAfter, true
		}
	}
	// Raw HTTP/2 backpressure that never got wrapped in a gRPC status.
	var se http2.StreamError
	if errors.As(err, &se) && se.Code == http2.ErrCodeEnhanceYourCalm {
		return "enhance_your_calm", retryAfter, true
	}
	var ga http2.GoAwayError
	if errors.As(err, &ga) && ga.ErrCode == http2.ErrCodeEnhanceYourCalm {
		return "enhance_your_calm", retryAfter, true
	}
	return "", 0, false
}

// nextDelay returns the nominal (pre-jitter) backoff after a transient error:
// 2*base on the first — so equal jitter lands in [base, 2*base] and we never
// retry FASTER than a normal poll right after the server asked us to slow
// down — doubling thereafter, capped at max. prev<=0 resets to the first value
// (used after a clean poll). The cap is raised to 2*base when smaller, so even
// at the cap the jittered sleep stays >= base.
func nextDelay(prev, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if max < 2*base {
		max = 2 * base
	}
	d := 2 * base
	if prev > 0 {
		d = prev * 2
	}
	if d > max {
		d = max
	}
	return d
}

// equalJitter spreads a nominal delay over [d/2, d]: a floor of half (so we
// genuinely back off) plus randomness (so a fleet of agents recovering from
// the same churn burst does not retry in lockstep and re-flood the apiserver).
func equalJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1)) //nolint:gosec // jitter spread, not security
}

// sleepCtx sleeps d, or returns ctx.Err() early if ctx is done. The timer is
// always stopped.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoffCfg carries the tunables/seams for pollWithBackoffImpl so tests inject
// a deterministic sleep/jitter WITHOUT a mutable package global — a global
// would race across the concurrently-running waiters (e.g. parallel service
// readiness polls), and then -race would prove nothing.
type backoffCfg struct {
	max    time.Duration
	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

func defaultBackoffCfg() backoffCfg {
	return backoffCfg{max: maxBackoff, sleep: sleepCtx, jitter: equalJitter}
}

// pollWithBackoff polls cond at `base` interval until it reports done or ctx is
// cancelled/expired. A transient backpressure error from cond
// (classifyTransientAPIErr) is RETRIED with capped exponential backoff + equal
// jitter — the correct response to an ENHANCE_YOUR_CALM "slow down" — while a
// non-transient error aborts immediately (preserving prior fail-fast on
// NotFound/Forbidden/etc.). `op` labels the retry metric with a closed-set
// waiter name.
//
// Drop-in replacement for wait.PollUntilContextCancel(ctx, base, true, cond):
// identical happy path (immediate first poll, steady `base` cadence), resilient
// error path.
func pollWithBackoff(ctx context.Context, op string, base time.Duration, cond wait.ConditionWithContextFunc) error {
	return pollWithBackoffImpl(ctx, op, base, defaultBackoffCfg(), cond)
}

func pollWithBackoffImpl(ctx context.Context, op string, base time.Duration, cfg backoffCfg, cond wait.ConditionWithContextFunc) error {
	if base <= 0 {
		base = time.Second
	}
	backoff := time.Duration(0) // 0 => not currently backing off
	for {
		done, err := cond(ctx)
		if err != nil {
			kind, retryAfter, ok := classifyTransientAPIErr(err)
			if !ok {
				return err // non-transient: abort (today's behaviour)
			}
			backoff = nextDelay(backoff, base, cfg.max)
			d := cfg.jitter(backoff)
			if d < base {
				d = base // never retry faster than a normal poll
			}
			if retryAfter > 0 {
				// Retry-After is a FLOOR, not the exact delay: add bounded
				// dispersion so a fleet hit by the same 429/APF doesn't wake in
				// lockstep and immediately re-flood the apiserver.
				if floored := retryAfter + cfg.jitter(base); floored > d {
					d = floored
				}
			}
			if serr := cfg.sleep(ctx, d); serr != nil {
				return serr // ctx expired during backoff: no retry happened
			}
			// Count only an ACTUAL retry — the next iteration re-polls. Placing
			// this after the sleep keeps *_retries_total from over-counting a
			// blip the ctx cancelled before we could retry.
			metrics.K8sTransientRetries.WithLabelValues(op, kind).Inc()
			continue
		}
		if done {
			return nil
		}
		backoff = 0 // clean poll: reset to steady base cadence
		if serr := cfg.sleep(ctx, base); serr != nil {
			return serr
		}
	}
}
