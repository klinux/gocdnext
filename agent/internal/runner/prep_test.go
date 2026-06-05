package runner

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

func TestPrep_HappyPath_ArtifactDownload(t *testing.T) {
	// Build a tar.gz that DownloadArtifact will pull + extract.
	tarPayload, sha := buildTarPayload(t, map[string]string{
		"hello.txt":     "world",
		"sub/inner.txt": "deep",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarPayload)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	a := &gocdnextv1.JobAssignment{
		RunId: "r1", JobId: "j1", Name: "test",
		ArtifactDownloads: []*gocdnextv1.ArtifactDownload{
			{
				Path:          "dist",
				GetUrl:        srv.URL,
				ContentSha256: sha,
				FromJob:       "upstream",
				Dest:          ".",
			},
		},
	}

	var logs bytes.Buffer
	if err := Prep(context.Background(), a, tmp, &logs); err != nil {
		t.Fatalf("prep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "hello.txt")); err != nil {
		t.Errorf("missing hello.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "sub", "inner.txt")); err != nil {
		t.Errorf("missing sub/inner.txt: %v", err)
	}
	if !strings.Contains(logs.String(), "prep: starting") {
		t.Errorf("expected start log; got %q", logs.String())
	}
	if !strings.Contains(logs.String(), "download artifact dist") {
		t.Errorf("expected download log line; got %q", logs.String())
	}
}

func TestPrep_NilAssignment(t *testing.T) {
	if err := Prep(context.Background(), nil, "/tmp", nil); err == nil {
		t.Fatal("expected nil assignment error")
	}
}

func TestPrep_EmptyWorkspaceDir(t *testing.T) {
	if err := Prep(context.Background(), &gocdnextv1.JobAssignment{}, "", nil); err == nil {
		t.Fatal("expected empty workspace error")
	}
}

func TestPrep_CacheMissIsSilent(t *testing.T) {
	// Literal-key cache miss (fetch_found=false) is the normal
	// cold-start case. Prep should NOT spam a warning for it —
	// only templated keys (not yet supported) deserve the
	// warning.
	tmp := t.TempDir()
	a := &gocdnextv1.JobAssignment{
		RunId: "r", JobId: "j",
		Caches: []*gocdnextv1.CacheEntry{
			{
				Key:        "pnpm-store-abc",
				Paths:      []string{".pnpm-store"},
				FetchFound: false,
			},
		},
	}
	var logs bytes.Buffer
	if err := Prep(context.Background(), a, tmp, &logs); err != nil {
		t.Fatalf("prep: %v", err)
	}
	if strings.Contains(logs.String(), "warning") {
		t.Errorf("expected no warning on cold-start miss; got: %s", logs.String())
	}
}

func TestPrep_LogsTemplatedKeyLimitation(t *testing.T) {
	// Templated cache keys are skipped in isolated mode (no
	// workspace-side hashing yet) with an explicit warning so
	// the operator understands why their pnpm-store-{{ hash ...
	// }} cache isn't restoring.
	tmp := t.TempDir()
	a := &gocdnextv1.JobAssignment{
		RunId: "r", JobId: "j",
		Caches: []*gocdnextv1.CacheEntry{
			{
				Key:        `pnpm-store-{{ hash "pnpm-lock.yaml" }}`,
				Paths:      []string{".pnpm-store"},
				FetchFound: false,
			},
		},
	}
	var logs bytes.Buffer
	if err := Prep(context.Background(), a, tmp, &logs); err != nil {
		t.Fatalf("prep: %v", err)
	}
	if !strings.Contains(logs.String(), "templated keys aren't yet supported") {
		t.Errorf("expected templated-key warning, got: %s", logs.String())
	}
}

func TestPrep_CacheHitDownloads(t *testing.T) {
	tarPayload, sha := buildTarPayload(t, map[string]string{
		"node_modules/foo.txt": "cached",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarPayload)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	a := &gocdnextv1.JobAssignment{
		RunId: "r", JobId: "j",
		Caches: []*gocdnextv1.CacheEntry{
			{
				Key:          "node-modules-abc",
				Paths:        []string{"node_modules"},
				FetchUrl:     srv.URL,
				FetchSha256:  sha,
				FetchFound:   true,
			},
		},
	}
	var logs bytes.Buffer
	if err := Prep(context.Background(), a, tmp, &logs); err != nil {
		t.Fatalf("prep: %v", err)
	}
	if !strings.Contains(logs.String(), "restored") {
		t.Errorf("expected cache restored log, got: %s", logs.String())
	}
}

func TestDownloadArtifact_RejectsBadSha(t *testing.T) {
	tarPayload, _ := buildTarPayload(t, map[string]string{"x.txt": "yes"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarPayload)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	dl := &gocdnextv1.ArtifactDownload{
		Path: "dist", GetUrl: srv.URL,
		ContentSha256: "deadbeef" + strings.Repeat("0", 56),
		FromJob:       "upstream",
		Dest:          ".",
	}
	err := DownloadArtifact(context.Background(), tmp, dl, nil)
	if err == nil {
		t.Fatal("expected sha mismatch error")
	}
}

func TestDownloadArtifact_RejectsNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dl := &gocdnextv1.ArtifactDownload{
		Path: "x", GetUrl: srv.URL,
		FromJob: "upstream",
	}
	if err := DownloadArtifact(context.Background(), t.TempDir(), dl, nil); err == nil {
		t.Fatal("expected non-2xx error")
	}
}

func TestDownloadArtifact_RejectsMissingURL(t *testing.T) {
	dl := &gocdnextv1.ArtifactDownload{Path: "x"}
	if err := DownloadArtifact(context.Background(), t.TempDir(), dl, nil); err == nil {
		t.Fatal("expected missing url error")
	}
}

func TestCheckout_RejectsMissingURL(t *testing.T) {
	mc := &gocdnextv1.MaterialCheckout{TargetDir: "src"}
	if err := Checkout(context.Background(), t.TempDir(), mc, nil); err == nil {
		t.Fatal("expected missing url error")
	}
}

// buildTarPayload builds an in-memory gzip-tar containing the given
// path→content pairs and returns the bytes + its sha256 hex.
func buildTarPayload(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()

	// Stage files on disk, then use TarGzPaths to build the archive
	// (same code path UntarGz reverses).
	stage := t.TempDir()
	pathsList := make([]string, 0, len(files))
	for p, content := range files {
		full := filepath.Join(stage, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		pathsList = append(pathsList, p)
	}

	var buf bytes.Buffer
	sha, _, err := TarGzPaths(stage, pathsList, &buf)
	if err != nil {
		t.Fatalf("tar: %v", err)
	}
	return buf.Bytes(), sha
}
