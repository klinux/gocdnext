package logstream

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBroker_DeliversEventToSubscriber(t *testing.T) {
	b := New(8, nil)
	runID := uuid.New()
	sub := b.Subscribe(runID)
	defer sub.Close()

	ev := Event{RunID: runID, JobRunID: uuid.New(), Seq: 1, Stream: "stdout", At: time.Now(), Text: "hello"}
	b.Publish(ev)

	select {
	case got := <-sub.C:
		if got.Text != "hello" {
			t.Errorf("want %q got %q", "hello", got.Text)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("event never arrived")
	}
}

func TestBroker_FanOutToMultipleSubscribersSameRun(t *testing.T) {
	b := New(8, nil)
	runID := uuid.New()
	a := b.Subscribe(runID)
	defer a.Close()
	c := b.Subscribe(runID)
	defer c.Close()

	b.Publish(Event{RunID: runID, Seq: 7, Text: "broadcast"})

	for _, sub := range []*Subscriber{a, c} {
		select {
		case got := <-sub.C:
			if got.Seq != 7 {
				t.Errorf("want seq 7, got %d", got.Seq)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("fan-out missed a subscriber")
		}
	}
}

func TestBroker_IsolatesRuns(t *testing.T) {
	b := New(8, nil)
	runA := uuid.New()
	runC := uuid.New()
	a := b.Subscribe(runA)
	defer a.Close()
	c := b.Subscribe(runC)
	defer c.Close()

	b.Publish(Event{RunID: runA, Seq: 1, Text: "only-a"})

	select {
	case got := <-a.C:
		if got.Text != "only-a" {
			t.Errorf("A got %q", got.Text)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("A should have received its event")
	}

	select {
	case got := <-c.C:
		t.Fatalf("C should have received nothing; got %+v", got)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestBroker_UnsubscribeStopsDelivery(t *testing.T) {
	b := New(8, nil)
	runID := uuid.New()
	sub := b.Subscribe(runID)

	sub.Close()

	// A second close is a no-op, not a panic. Handlers commonly
	// defer Close without knowing if the stream already ended.
	sub.Close()

	if n := b.SubscriberCount(runID); n != 0 {
		t.Errorf("expected 0 subs after close, got %d", n)
	}

	b.Publish(Event{RunID: runID, Text: "noone"})

	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("received event after close")
		}
		// Closed channel yields zero-value with ok=false — expected.
	case <-time.After(30 * time.Millisecond):
		t.Fatal("closed channel should be drainable immediately")
	}
}

func TestBroker_SlowSubscriberDroppedNotBlocked(t *testing.T) {
	var dropped atomic.Int64
	b := New(2, func() { dropped.Add(1) })
	runID := uuid.New()
	sub := b.Subscribe(runID)
	defer sub.Close()

	// Fill the buffer, then push one extra — that extra MUST drop
	// rather than block Publish.
	b.Publish(Event{RunID: runID, Seq: 1})
	b.Publish(Event{RunID: runID, Seq: 2})

	done := make(chan struct{})
	go func() {
		b.Publish(Event{RunID: runID, Seq: 3})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Publish blocked on a slow subscriber")
	}

	if dropped.Load() == 0 {
		t.Error("drop counter never incremented")
	}
}

func TestBroker_ConcurrentPublishIsSafe(t *testing.T) {
	b := New(1024, nil)
	runID := uuid.New()
	sub := b.Subscribe(runID)
	defer sub.Close()

	const senders = 16
	const per = 64
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(seq int64) {
			defer wg.Done()
			for j := 0; j < per; j++ {
				b.Publish(Event{RunID: runID, Seq: seq*1000 + int64(j)})
			}
		}(int64(i))
	}

	received := 0
	deadline := time.After(2 * time.Second)
	go func() { wg.Wait() }()

loop:
	for received < senders*per {
		select {
		case <-sub.C:
			received++
		case <-deadline:
			break loop
		}
	}
	// The buffer is large enough to hold every sent event, so we
	// expect none dropped.
	if received != senders*per {
		t.Errorf("received %d / want %d", received, senders*per)
	}
}
