// Package logstream is an in-process fan-out of log lines from
// the agent-ingest path to SSE subscribers. It keeps the server
// single-instance friendly: publishes are non-blocking, slow
// subscribers get dropped rather than backpressure the producer,
// and there's no DB round-trip per line.
//
// When gocdnext grows an HA story, this broker is where the
// cross-node piece plugs in (LISTEN/NOTIFY fan-in, Redis pubsub,
// or a NATS subject). The interface stays the same; only the
// Publish path gains a network hop.
package logstream

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Event is one log line as it crosses the broker. Matches the
// wire shape the SSE handler forwards to the browser — keep the
// field names stable so the JSON encoding is predictable.
type Event struct {
	RunID    uuid.UUID `json:"-"`
	JobRunID uuid.UUID `json:"job_id"`
	Seq      int64     `json:"seq"`
	Stream   string    `json:"stream"`
	At       time.Time `json:"at"`
	Text     string    `json:"text"`
}

// Subscriber is a single consumer's view of the broker. Close
// releases the underlying channel and evicts it from the fan-out
// — call it from `defer` on the handler that owns the stream.
type Subscriber struct {
	C      <-chan Event
	close  func()
	closed bool
	mu     sync.Mutex
}

func (s *Subscriber) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.close()
}

// Broker fans out log events by run_id. A single SSE handler
// subscribes with Subscribe(runID); each agent log line hits
// Publish(event) and lands in every subscriber's buffered
// channel. Zero value is NOT usable — call New.
type Broker struct {
	mu          sync.RWMutex
	byRun       map[uuid.UUID]map[*subscription]struct{}
	bufferSize  int
	dropCounter func() // optional hook for tests / metrics
}

type subscription struct {
	ch chan Event
}

// New builds a broker. bufferSize is the per-subscriber channel
// capacity — tune it for bursty log output (a job that prints
// many lines between two SSE flushes). 256 is a reasonable
// default: one job's pathological burst won't block ingest, and
// typical CI output sits well under that. onDrop is called
// (non-blocking) each time a publish skips a subscriber whose
// buffer is full; keep the callback fast.
func New(bufferSize int, onDrop func()) *Broker {
	if bufferSize < 1 {
		bufferSize = 256
	}
	return &Broker{
		byRun:       map[uuid.UUID]map[*subscription]struct{}{},
		bufferSize:  bufferSize,
		dropCounter: onDrop,
	}
}

// Subscribe registers a new consumer for `runID`. The returned
// Subscriber holds a receive-only channel of Events; call
// Subscriber.Close when done.
func (b *Broker) Subscribe(runID uuid.UUID) *Subscriber {
	sub := &subscription{ch: make(chan Event, b.bufferSize)}

	b.mu.Lock()
	subs := b.byRun[runID]
	if subs == nil {
		subs = map[*subscription]struct{}{}
		b.byRun[runID] = subs
	}
	subs[sub] = struct{}{}
	b.mu.Unlock()

	s := &Subscriber{C: sub.ch}
	s.close = func() {
		b.mu.Lock()
		if m, ok := b.byRun[runID]; ok {
			delete(m, sub)
			if len(m) == 0 {
				delete(b.byRun, runID)
			}
		}
		b.mu.Unlock()
		close(sub.ch)
	}
	return s
}

// Publish hands `ev` to every subscriber of ev.RunID. Slow
// subscribers — buffer full — are skipped (dropCounter called)
// so ingest never blocks on a stalled SSE connection. The UI
// is expected to recover by refetching the backlog once it's
// caught up; losing a live line on a saturated stream is the
// correct trade.
func (b *Broker) Publish(ev Event) {
	b.mu.RLock()
	subs := b.byRun[ev.RunID]
	// Snapshot the slice of channels while under the read lock so
	// Publish doesn't hold the lock across a send.
	targets := make([]*subscription, 0, len(subs))
	for s := range subs {
		targets = append(targets, s)
	}
	b.mu.RUnlock()

	for _, s := range targets {
		select {
		case s.ch <- ev:
		default:
			if b.dropCounter != nil {
				b.dropCounter()
			}
		}
	}
}

// SubscriberCount is a test helper — not a public surface for
// production code. Only use it from tests (hence the internal
// naming; nothing stops it from being called but the name makes
// intent clear).
func (b *Broker) SubscriberCount(runID uuid.UUID) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.byRun[runID])
}
