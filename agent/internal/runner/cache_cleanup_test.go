package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// capturingExec is a PodExecutor that records the argv it was asked to run.
type capturingExec struct {
	calls [][]string
	err   error
}

var _ engine.PodExecutor = (*capturingExec)(nil)

func (c *capturingExec) Exec(_ context.Context, _, _ string, cmd []string, _ io.Reader, _, _ io.Writer) error {
	c.calls = append(c.calls, append([]string(nil), cmd...))
	return c.err
}

// A failed isolated restore must wipe the declared paths (so a truncated untar
// can't leave a corrupt tree), via a shell-free `rm -rf --` argv.
func TestWipePodCachePaths(t *testing.T) {
	ex := &capturingExec{}
	err := wipePodCachePaths(context.Background(), ex, "pod", "/workspace/src/x",
		[]string{".gradle-home/caches", "", ".gradle-home/wrapper"})
	if err != nil {
		t.Fatalf("wipePodCachePaths: %v", err)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("want exactly 1 exec, got %d (%v)", len(ex.calls), ex.calls)
	}
	want := []string{"rm", "-rf", "--",
		"/workspace/src/x/.gradle-home/caches",
		"/workspace/src/x/.gradle-home/wrapper"}
	if !slices.Equal(ex.calls[0], want) {
		t.Errorf("argv = %v, want %v", ex.calls[0], want)
	}
	if ex.calls[0][0] != "rm" {
		t.Errorf("must exec rm directly (argv, no shell), got %q", ex.calls[0][0])
	}

	// No paths → no exec at all.
	ex2 := &capturingExec{}
	if err := wipePodCachePaths(context.Background(), ex2, "pod", "/ws", nil); err != nil {
		t.Fatalf("empty paths: %v", err)
	}
	if len(ex2.calls) != 0 {
		t.Errorf("empty paths must not exec, got %v", ex2.calls)
	}
}

// A failed shared-mode restore must remove the partial content under the
// declared paths while leaving unrelated workspace content untouched.
func TestWipePartialCache(t *testing.T) {
	wd := t.TempDir()
	// Simulate a half-unpacked Gradle dist left by a truncated untar.
	partial := filepath.Join(wd, ".gradle-home", "wrapper", "dists", "gradle-9.4.0", "bin")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "gradle"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Unrelated content (the source checkout) must survive the wipe.
	keep := filepath.Join(wd, "src")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}

	r := &Runner{}
	e := &gocdnextv1.CacheEntry{
		Key:   "gradle-deps-x",
		Paths: []string{".gradle-home/caches", ".gradle-home/wrapper"},
	}
	r.wipePartialCache(wd, e, &gocdnextv1.JobAssignment{}, &atomic.Int64{})

	if _, err := os.Stat(filepath.Join(wd, ".gradle-home", "wrapper")); !os.IsNotExist(err) {
		t.Errorf(".gradle-home/wrapper must be gone after wipe, stat err = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated src/ must survive the wipe, got %v", err)
	}
}

// On a failed restore, the sha already computed inline over the streamed body
// discriminates a truncated TRANSFER (received sha != stored → transient,
// retriable) from a corrupt STORED blob (full bytes, sha matches, but the
// decoder still fails → not retriable). The fake exec fails the "untar" so the
// discrimination path runs; the drain re-reads the body through the hasher.
func TestStreamCacheIntoPod_DiscriminatesTransferVsStored(t *testing.T) {
	// Leading gzip magic so podUntarCmd picks a codec and reaches the exec;
	// the body content is irrelevant because the fake exec always fails.
	body := append([]byte{0x1f, 0x8b}, []byte("not-a-real-archive")...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	sum := sha256.Sum256(body)
	fullSha := hex.EncodeToString(sum[:])

	ex := &capturingExec{err: errors.New("untar boom")}

	// Full body arrives and its sha matches the stored sha → STORED-corrupt.
	err := streamCacheIntoPod(context.Background(), ex, "pod", "/ws", srv.URL, fullSha)
	var cre *cacheRestoreError
	if !errors.As(err, &cre) {
		t.Fatalf("want *cacheRestoreError, got %T: %v", err, err)
	}
	if cre.transientTransfer {
		t.Errorf("full body + matching sha must classify as STORED-corrupt (transient=false)")
	}

	// Stored sha differs from what was received → TRANSFER truncated.
	err = streamCacheIntoPod(context.Background(), ex, "pod", "/ws", srv.URL,
		"00000000000000000000000000000000000000000000000000000000deadbeef")
	if !errors.As(err, &cre) {
		t.Fatalf("want *cacheRestoreError, got %T: %v", err, err)
	}
	if !cre.transientTransfer {
		t.Errorf("sha mismatch must classify as TRANSFER-truncated (transient=true)")
	}
}
