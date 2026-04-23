package runner_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"

	"github.com/gocdnext/gocdnext/agent/internal/runner"
)

// fakeCache lets tests drive the runner's cache lifecycle without
// a gRPC stack. Fetch+Store are programmed per-test via the
// public fields; calls are counted + captured for assertions.
type fakeCache struct {
	mu sync.Mutex

	fetchFound bool
	fetchErr   error
	// fetchWrite: if non-nil, runs inside Fetch against workDir so
	// a successful fetch can materialise real files on disk (the
	// runner test asserts that a cache-hit pre-populates a dir
	// before the task sees it).
	fetchWrite func(workDir string) error

	storeErr error

	fetchCalls []cacheCall
	storeCalls []cacheCall
}

type cacheCall struct {
	WorkDir string
	Key     string
	Paths   []string
}

func (f *fakeCache) Fetch(ctx context.Context, workDir, runID, jobID string, entry *gocdnextv1.CacheEntry) (bool, error) {
	f.mu.Lock()
	f.fetchCalls = append(f.fetchCalls, cacheCall{workDir, entry.GetKey(), entry.GetPaths()})
	f.mu.Unlock()
	if f.fetchErr != nil {
		return false, f.fetchErr
	}
	if f.fetchFound && f.fetchWrite != nil {
		if err := f.fetchWrite(workDir); err != nil {
			return false, err
		}
	}
	return f.fetchFound, nil
}

func (f *fakeCache) Store(ctx context.Context, workDir, runID, jobID string, entry *gocdnextv1.CacheEntry) error {
	f.mu.Lock()
	f.storeCalls = append(f.storeCalls, cacheCall{workDir, entry.GetKey(), entry.GetPaths()})
	f.mu.Unlock()
	return f.storeErr
}

// newRunnerWithCache is newRunner + an injected CacheClient. Kept
// separate so the existing runner tests don't need to change.
func newRunnerWithCache(t *testing.T, fc *fakeCache) (*runner.Runner, *collector) {
	t.Helper()
	c := &collector{}
	r := runner.New(runner.Config{
		WorkspaceRoot: t.TempDir(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send:          c.Send,
		Cache:         fc,
	})
	return r, c
}

func assignmentWithCache(entries []*gocdnextv1.CacheEntry, tasks ...string) *gocdnextv1.JobAssignment {
	a := assignment(tasks...)
	a.Caches = entries
	return a
}

func TestExecute_CacheFetchHitBeforeTasks(t *testing.T) {
	// A cache-hit must run BEFORE the task script. The fake's
	// fetchWrite drops a sentinel file and the script greps for
	// it — if fetchCaches ran after tasks, `cat` would fail.
	fc := &fakeCache{
		fetchFound: true,
		fetchWrite: func(workDir string) error {
			return os.WriteFile(filepath.Join(workDir, "restored.txt"), []byte("ok"), 0o644)
		},
	}
	r, c := newRunnerWithCache(t, fc)

	a := assignmentWithCache(
		[]*gocdnextv1.CacheEntry{{Key: "pnpm-store", Paths: []string{".pnpm-store"}}},
		"cat restored.txt",
	)
	r.Execute(context.Background(), a)

	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("job did not succeed: %+v", c.result)
	}
	if !strings.Contains(c.allLogText(), "restored 1 path(s)") {
		t.Errorf("expected restored log line; got:\n%s", c.allLogText())
	}
	if len(fc.fetchCalls) != 1 || fc.fetchCalls[0].Key != "pnpm-store" {
		t.Errorf("fetch calls = %+v", fc.fetchCalls)
	}
}

func TestExecute_CacheFetchMissLogsAndContinues(t *testing.T) {
	// Cold start: Fetch returns found=false. The runner must log
	// "miss, no pre-populated dir" and carry on to run the task
	// (which shouldn't fail just because there's no cache).
	fc := &fakeCache{fetchFound: false}
	r, c := newRunnerWithCache(t, fc)

	r.Execute(context.Background(), assignmentWithCache(
		[]*gocdnextv1.CacheEntry{{Key: "go-build", Paths: []string{".go-cache"}}},
		"echo still-ran",
	))
	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("job status = %+v", c.result)
	}
	logs := c.allLogText()
	if !strings.Contains(logs, `cache "go-build": miss`) {
		t.Errorf("expected miss log; got:\n%s", logs)
	}
	if !strings.Contains(logs, "still-ran") {
		t.Errorf("task did not run: %s", logs)
	}
}

func TestExecute_CacheFetchErrorIsNonFatal(t *testing.T) {
	// A transport error (bad signed URL, sha mismatch, etc.) must
	// NOT fail the job — cache is acceleration, not correctness.
	// Log loudly, but the task still runs and the result is
	// success if the task itself succeeds.
	fc := &fakeCache{fetchErr: errors.New("boom: sha mismatch")}
	r, c := newRunnerWithCache(t, fc)

	r.Execute(context.Background(), assignmentWithCache(
		[]*gocdnextv1.CacheEntry{{Key: "k", Paths: []string{"x"}}},
		"true",
	))
	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("transport err should not fail job: %+v", c.result)
	}
	if !strings.Contains(c.allLogText(), "fetch failed (boom: sha mismatch)") {
		t.Errorf("expected fetch-failure log; got:\n%s", c.allLogText())
	}
}

func TestExecute_CacheStoreRunsAfterSuccess(t *testing.T) {
	fc := &fakeCache{}
	r, c := newRunnerWithCache(t, fc)

	r.Execute(context.Background(), assignmentWithCache(
		[]*gocdnextv1.CacheEntry{{Key: "pnpm-store", Paths: []string{".pnpm-store"}}},
		"true",
	))
	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("job did not succeed: %+v", c.result)
	}
	if len(fc.storeCalls) != 1 || fc.storeCalls[0].Key != "pnpm-store" {
		t.Errorf("store calls = %+v", fc.storeCalls)
	}
}

func TestExecute_CacheStoreSkippedOnTaskFailure(t *testing.T) {
	// Don't cache a half-built tree: if tasks fail, storeCaches
	// must never be called. Otherwise the next run pre-populates
	// corrupt state and "but it worked last time" becomes "why
	// does this job always fail quickly now".
	fc := &fakeCache{}
	r, c := newRunnerWithCache(t, fc)

	r.Execute(context.Background(), assignmentWithCache(
		[]*gocdnextv1.CacheEntry{{Key: "pnpm-store", Paths: []string{".pnpm-store"}}},
		"false", // exit 1 → FAILED
	))
	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_FAILED {
		t.Fatalf("expected failure; got %+v", c.result)
	}
	if len(fc.storeCalls) != 0 {
		t.Errorf("store called on failed job: %+v", fc.storeCalls)
	}
}

func TestExecute_CacheStoreErrorIsNonFatal(t *testing.T) {
	fc := &fakeCache{storeErr: errors.New("s3 5xx")}
	r, c := newRunnerWithCache(t, fc)

	r.Execute(context.Background(), assignmentWithCache(
		[]*gocdnextv1.CacheEntry{{Key: "k", Paths: []string{"x"}}},
		"true",
	))
	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("store err should not fail job: %+v", c.result)
	}
	if !strings.Contains(c.allLogText(), "store failed (s3 5xx)") {
		t.Errorf("expected store-failure log; got:\n%s", c.allLogText())
	}
}

func TestExecute_NoCacheClientNoOp(t *testing.T) {
	// Agent built without a CacheClient (test doubles, shell-only
	// deploys) must silently ignore the caches slice on the
	// assignment. No panic, no log, no side effect.
	r, c := newRunner(t)

	r.Execute(context.Background(), assignmentWithCache(
		[]*gocdnextv1.CacheEntry{{Key: "k", Paths: []string{"x"}}},
		"echo ok",
	))
	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("no-cache happy path should succeed: %+v", c.result)
	}
	if strings.Contains(c.allLogText(), "cache ") {
		t.Errorf("cache messages logged when CacheClient is nil:\n%s", c.allLogText())
	}
}

func TestTarGzPaths_MultiplePathsInOneArchive(t *testing.T) {
	// The cache key addresses a bundle (.pnpm-store +
	// node_modules under one key). Extract order doesn't matter
	// but both must be present.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "x.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	sha, size, err := runner.TarGzPaths(dir, []string{"a", "b.txt"}, &buf)
	if err != nil {
		t.Fatalf("TarGzPaths: %v", err)
	}
	if size <= 0 || len(sha) != 64 {
		t.Errorf("sha=%q size=%d", sha, size)
	}

	gr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gr)
	seen := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		body, _ := io.ReadAll(tr)
		seen[h.Name] = string(body)
	}
	if seen["a/x.txt"] != "one" || seen["b.txt"] != "two" {
		t.Errorf("missing entries: %+v", seen)
	}
}

func TestTarGzPaths_SkipsMissingPaths(t *testing.T) {
	// node_modules may not exist yet on cold start — declared
	// paths that aren't materialised must be SKIPPED, not errored.
	// Otherwise the first "store after cold start" always fails
	// and a cache never lands.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "present.txt"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	_, _, err := runner.TarGzPaths(dir, []string{"present.txt", "never_here"}, &buf)
	if err != nil {
		t.Errorf("missing path should skip, got err: %v", err)
	}

	gr, _ := gzip.NewReader(&buf)
	tr := tar.NewReader(gr)
	count := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		count++
	}
	if count != 1 {
		t.Errorf("entries = %d, want 1 (only present.txt)", count)
	}
}

func TestDownloadAndUntar_RoundTripsBytes(t *testing.T) {
	// End-to-end at the helper level: tar+gz some files, serve
	// them over a test HTTP server, DownloadAndUntar into a fresh
	// dir. Verifies the sha check passes and every byte round-
	// trips — proves Fetch+Store don't lose content on good paths.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	var payload bytes.Buffer
	sha, _, err := runner.TarGzPaths(src, []string{"file.txt"}, &payload)
	if err != nil {
		t.Fatalf("tar: %v", err)
	}

	dest := t.TempDir()
	url, cleanup := serveBytes(t, payload.Bytes())
	defer cleanup()
	if err := runner.DownloadAndUntar(context.Background(), nil, url, dest, sha); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "file.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("content = %q", got)
	}
}

// serveBytes spins up a one-shot HTTP server that returns b for
// GET / and tears down in the cleanup. Kept local to keep the
// test self-contained — reaching into runner's own http.Client
// would miss the signed-URL shape the real code uses.
func serveBytes(t *testing.T, b []byte) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(b)
	}))
	return srv.URL, srv.Close
}
