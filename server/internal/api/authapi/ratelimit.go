package authapi

import (
	"sync"
	"time"
)

// loginRateLimiter is a small in-process sliding-window counter
// for /auth/login/local attempts. Scope is per-key (we use
// lowercase email) so an attacker switching IPs still hits the
// same bucket. Memory-only — sufficient for a single-binary CI
// tool where local users are the break-glass path, not the
// primary auth surface. A clustered deployment would want Redis.
type loginRateLimiter struct {
	mu       sync.Mutex
	failures map[string]*failureBucket
	// config
	window      time.Duration
	maxFailures int
	lockFor     time.Duration
	// clock is an injection point for tests; nil means time.Now.
	clock func() time.Time
}

type failureBucket struct {
	attempts  []time.Time
	lockUntil time.Time
}

// newLoginRateLimiter picks reasonable defaults for break-glass
// traffic: 5 failures in 10 min locks the bucket for 10 min.
// Legit admins who fat-finger a password twice won't trip it;
// a scanner does, and pays a 10-minute cooldown.
func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{
		failures:    map[string]*failureBucket{},
		window:      10 * time.Minute,
		maxFailures: 5,
		lockFor:     10 * time.Minute,
	}
}

func (l *loginRateLimiter) now() time.Time {
	if l.clock != nil {
		return l.clock()
	}
	return time.Now()
}

// Allow reports whether a login attempt for `key` should proceed
// right now. On false, the handler returns 429 without even
// touching bcrypt.
func (l *loginRateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.failures[key]
	if b == nil {
		return true
	}
	now := l.now()
	if now.Before(b.lockUntil) {
		return false
	}
	// Clean stale attempts while we're here (opportunistic GC).
	cutoff := now.Add(-l.window)
	fresh := b.attempts[:0]
	for _, t := range b.attempts {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	b.attempts = fresh
	return true
}

// RecordFailure bumps the counter and trips the lockout when the
// bucket crosses the threshold.
func (l *loginRateLimiter) RecordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.failures[key]
	if b == nil {
		b = &failureBucket{}
		l.failures[key] = b
	}
	now := l.now()
	cutoff := now.Add(-l.window)
	fresh := b.attempts[:0]
	for _, t := range b.attempts {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	b.attempts = append(fresh, now)
	if len(b.attempts) >= l.maxFailures {
		b.lockUntil = now.Add(l.lockFor)
	}
}

// RecordSuccess clears the bucket — a legit login shouldn't leave
// a trail that trips the next typo.
func (l *loginRateLimiter) RecordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}
