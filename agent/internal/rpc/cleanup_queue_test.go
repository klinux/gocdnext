package rpc_test

import (
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/rpc"
)

// TestEnqueueRunCleanup_Coalesces — the same runID enqueued
// multiple times must result in exactly ONE pending entry +
// ONE queued item. Server broadcast routinely sends the same
// CleanupRunServices to the same agent (e.g., ranAgents and
// AllAgentIDs(k8s) sets overlap), so without coalescing we'd
// waste worker cycles on idempotent duplicates AND inflate the
// queue toward saturation faster than necessary.
func TestEnqueueRunCleanup_Coalesces(t *testing.T) {
	c := rpc.New(rpc.Config{
		ServerAddr: "ignored",
		AgentID:    "a", Token: "t",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const runID = "run-123"
	c.EnqueueRunCleanupForTest(runID)
	c.EnqueueRunCleanupForTest(runID)
	c.EnqueueRunCleanupForTest(runID)

	if got := c.CleanupPendingLenForTest(); got != 1 {
		t.Errorf("pending len = %d, want 1 (coalesce by runID)", got)
	}
	if got := c.CleanupQueueLenForTest(); got != 1 {
		t.Errorf("queue len = %d, want 1 (coalesce by runID)", got)
	}
}

// TestEnqueueRunCleanup_SaturationDrops — when the bounded queue
// fills up with distinct runIDs, additional enqueues are dropped
// (logged but not stored). This is the only path to a leak in the
// agent-side design — capacity is large enough that fleet-wide
// cancel bursts don't reach it in practice. Test asserts the
// invariant: capacity items land, the next one doesn't.
func TestEnqueueRunCleanup_SaturationDrops(t *testing.T) {
	c := rpc.New(rpc.Config{
		ServerAddr: "ignored",
		AgentID:    "a", Token: "t",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cap := c.CleanupQueueCapForTest()
	for i := 0; i < cap; i++ {
		c.EnqueueRunCleanupForTest("r-" + strconv.Itoa(i))
	}
	if got := c.CleanupPendingLenForTest(); got != cap {
		t.Fatalf("pending after fill = %d, want %d", got, cap)
	}

	// Saturated: this one drops.
	c.EnqueueRunCleanupForTest("r-overflow")
	if got := c.CleanupPendingLenForTest(); got != cap {
		t.Errorf("pending after overflow = %d, want %d (overflow must NOT enter pending)", got, cap)
	}
	if got := c.CleanupQueueLenForTest(); got != cap {
		t.Errorf("queue after overflow = %d, want %d", got, cap)
	}
}

// TestEnqueueRunCleanup_EmptyRunIDIgnored — defensive guard against
// a malformed ServerMessage carrying an empty run_id reaching the
// queue and triggering a delete-all label-selector hit.
func TestEnqueueRunCleanup_EmptyRunIDIgnored(t *testing.T) {
	c := rpc.New(rpc.Config{
		ServerAddr: "ignored",
		AgentID:    "a", Token: "t",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	c.EnqueueRunCleanupForTest("")
	if got := c.CleanupPendingLenForTest(); got != 0 {
		t.Errorf("empty runID accepted: pending=%d, want 0", got)
	}
	if got := c.CleanupQueueLenForTest(); got != 0 {
		t.Errorf("empty runID queued: %d, want 0", got)
	}
}
