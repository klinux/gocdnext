package runner

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// markerExec records every command argv it received so the tests
// can assert on the exact shape of the touch + mkdir calls. Used
// to pin down the HIGH-2 shell-injection fix (no `sh -c` allowed
// in the marker touch path).
type markerExec struct {
	mu       sync.Mutex
	allCalls [][]string
	errFor   map[string]error // by cmd[0]
}

var _ engine.PodExecutor = (*markerExec)(nil)

func (m *markerExec) Exec(_ context.Context, _, _ string, cmd []string,
	_ io.Reader, _, stderr io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allCalls = append(m.allCalls, append([]string(nil), cmd...))
	if e, ok := m.errFor[cmd[0]]; ok {
		_, _ = stderr.Write([]byte("simulated"))
		return e
	}
	return nil
}

// TestTouchMarker_UsesArgvFormNotShellConcat is the HIGH-2
// regression cover. Pre-fix, touchCacheReadyMarker did
// `sh -c "mkdir -p $(dirname " + marker + ") && touch " + marker"`
// — operator-controlled marker (via target_dir / mountPath) was
// concatenated into a shell script. After the fix, two separate
// argv-form exec calls: `mkdir -p <dir>` and `touch <file>`. No
// shell, no metacharacter interpretation, no quoting bugs, and
// paths with spaces work.
func TestTouchMarker_UsesArgvFormNotShellConcat(t *testing.T) {
	exec := &markerExec{}
	if err := touchCacheReadyMarker(context.Background(), exec, "pod", "/workspace"); err != nil {
		t.Fatalf("touch marker: %v", err)
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()

	if len(exec.allCalls) != 2 {
		t.Fatalf("want exactly 2 execs (mkdir + touch), got %d: %v", len(exec.allCalls), exec.allCalls)
	}
	// No call may be `sh -c …` — that's the bug.
	for _, call := range exec.allCalls {
		if call[0] == "sh" {
			t.Errorf("found `sh -c` call in marker path: %v (shell-concat regression)", call)
		}
	}

	// First call is mkdir -p <dir>; second is touch <file>.
	mkdir, touch := exec.allCalls[0], exec.allCalls[1]
	if mkdir[0] != "mkdir" || mkdir[1] != "-p" {
		t.Errorf("first call shape = %v, want mkdir -p <dir>", mkdir)
	}
	if touch[0] != "touch" || len(touch) != 2 {
		t.Errorf("second call shape = %v, want touch <file>", touch)
	}
}

// TestTouchMarker_PathDerivesFromMountPathNotWorkDir is the HIGH-1
// regression cover. Pre-fix, the agent built the marker from the
// task's scriptWorkDir (which dives into target_dir), while the
// init container hardcoded `/workspace/...`. A target_dir job
// would touch `/workspace/<target_dir>/...` and the init container
// would block on `/workspace/...` until the job-level
// cancel/timeout. Now both go through CacheFetchMarkerPath fed by
// the SAME mountPath.
func TestTouchMarker_PathDerivesFromMountPathNotWorkDir(t *testing.T) {
	tests := []struct {
		name      string
		mountPath string
		want      string
	}{
		{"default", "/workspace", "/workspace/.gocdnext/cache-done"},
		{"custom", "/data/job", "/data/job/.gocdnext/cache-done"},
		{"trailing slash trimmed", "/workspace/", "/workspace/.gocdnext/cache-done"},
		{"empty defaults", "", "/workspace/.gocdnext/cache-done"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.CacheFetchMarkerPath(tc.mountPath)
			if got != tc.want {
				t.Errorf("CacheFetchMarkerPath(%q) = %q, want %q", tc.mountPath, got, tc.want)
			}
			// Sanity: same string must flow through touchCacheReadyMarker.
			exec := &markerExec{}
			if err := touchCacheReadyMarker(context.Background(), exec, "pod", tc.mountPath); err != nil {
				t.Fatalf("touch: %v", err)
			}
			exec.mu.Lock()
			defer exec.mu.Unlock()
			if len(exec.allCalls) < 2 || exec.allCalls[1][1] != tc.want {
				t.Errorf("touch invoked with %v, want last arg %q", exec.allCalls, tc.want)
			}
		})
	}
}

// TestTouchMarker_MkdirErrorPropagates — when mkdir fails, the
// caller doesn't even try the touch. Otherwise a missing dir
// would surface as a touch error referencing the absent dir,
// which is misleading.
func TestTouchMarker_MkdirErrorPropagates(t *testing.T) {
	exec := &markerExec{errFor: map[string]error{"mkdir": errors.New("permission denied")}}
	err := touchCacheReadyMarker(context.Background(), exec, "pod", "/workspace")
	if err == nil {
		t.Fatal("want error from mkdir failure, got nil")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("err should mention mkdir; got %v", err)
	}
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.allCalls) != 1 {
		t.Errorf("touch should be skipped after mkdir failure; got %d calls: %v",
			len(exec.allCalls), exec.allCalls)
	}
}
