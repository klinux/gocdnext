package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// tarBytes builds a one-file uncompressed tar for the codec round-trip tests.
func tarBytes(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// recordingExec captures the argv and drains stdin (so the agent-side
// TeeReader hashes the whole blob, mirroring a real tar consuming its input).
type recordingExec struct {
	mu   sync.Mutex
	cmds [][]string
}

func (r *recordingExec) Exec(_ context.Context, _, _ string, cmd []string,
	stdin io.Reader, _, _ io.Writer) error {
	if stdin != nil {
		_, _ = io.Copy(io.Discard, stdin)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, append([]string(nil), cmd...))
	return nil
}

// TestStreamCacheIntoPod_PicksCmdFromBlobMagic drives the real
// streamCacheIntoPod against an httptest server that serves either a gzip or a
// zstd blob, and asserts the in-pod command matches the blob's actual codec —
// the reader-first guarantee that a pre-existing gzip cache and a new zstd
// cache both restore through the same code (#274).
func TestStreamCacheIntoPod_PicksCmdFromBlobMagic(t *testing.T) {
	raw := tarBytes(t, "f.txt", "x")

	tests := []struct {
		name    string
		blob    []byte
		wantCmd string // substring that identifies the codec branch
	}{
		{"gzip", gz(raw), "tar -xzf -"},
		{"zstd", zst(t, raw), "zstd -dc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(tt.blob)
			}))
			defer srv.Close()

			exec := &recordingExec{}
			// expectedSha "" → skip verification; we only assert command choice.
			if err := streamCacheIntoPod(context.Background(), exec, "pod", "/workspace", srv.URL, ""); err != nil {
				t.Fatalf("streamCacheIntoPod: %v", err)
			}
			exec.mu.Lock()
			defer exec.mu.Unlock()
			if len(exec.cmds) != 1 {
				t.Fatalf("want 1 exec, got %d: %v", len(exec.cmds), exec.cmds)
			}
			joined := strings.Join(exec.cmds[0], " ")
			if !strings.Contains(joined, tt.wantCmd) {
				t.Errorf("cmd = %q, want it to contain %q", joined, tt.wantCmd)
			}
		})
	}
}

func gz(b []byte) []byte {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}

func zst(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}
