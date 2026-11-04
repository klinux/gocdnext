package groupsource

import (
	"sync"
	"time"
)

// Breaker tuning. Not operator knobs: a vote waits at most one Timeout per call, and
// these only decide how quickly a dead platform stops costing each vote that Timeout.
const (
	breakerThreshold = 5                // consecutive failures that open the circuit
	breakerCooldown  = 30 * time.Second // how long it stays open before one probe
)

// verdict is what a finished call says about the platform's health.
type verdict int

const (
	healthy   verdict = iota // answered (incl. 404): closes the circuit
	unhealthy                // failed: counts toward opening it
	neutral                  // says nothing (caller canceled): just ends a probe
)

// breaker is a consecutive-failure circuit breaker. Closed: every call goes through.
// Open (threshold reached): calls are refused until the cooldown passes. Half-open:
// one probe at a time goes through; its verdict closes or re-opens the circuit.
type breaker struct {
	mu        sync.Mutex
	now       func() time.Time
	failures  int
	openUntil time.Time
	probing   bool
}

func newBreaker() *breaker {
	return &breaker{now: time.Now}
}

// allow reports whether a call may go out. In the half-open state it admits a
// single probe; every call it admits must be followed by exactly one record.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures < breakerThreshold {
		return true
	}
	if b.probing || b.now().Before(b.openUntil) {
		return false
	}
	b.probing = true
	return true
}

func (b *breaker) record(v verdict) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	switch v {
	case healthy:
		b.failures = 0
	case unhealthy:
		b.failures++
		if b.failures >= breakerThreshold {
			b.openUntil = b.now().Add(breakerCooldown)
		}
	case neutral:
		// Neither health signal: an interrupted probe just frees the slot.
	}
}
