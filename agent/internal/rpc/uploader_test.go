package rpc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// stubAgentClient implements just enough of gocdnextv1.AgentServiceClient
// for the uploader's RequestArtifactUpload + PUT path to round-trip.
// Records the last request so the test can assert what hit the wire.
type stubAgentClient struct {
	gocdnextv1.AgentServiceClient // embed so methods we don't implement compile-error if called
	lastRequest                   *gocdnextv1.RequestArtifactUploadRequest
	tickets                       []*gocdnextv1.ArtifactUploadTicket
}

func (s *stubAgentClient) RequestArtifactUpload(
	_ context.Context,
	in *gocdnextv1.RequestArtifactUploadRequest,
	_ ...grpc.CallOption,
) (*gocdnextv1.RequestArtifactUploadResponse, error) {
	s.lastRequest = in
	return &gocdnextv1.RequestArtifactUploadResponse{Tickets: s.tickets}, nil
}

// TestUpload_DedupesPathsBeforeRPC — round-2 fix for the
// server/agent contract mismatch: the server now dedupes paths
// before issuing tickets (migration 00035 partial unique index
// requires it), so the agent MUST send canonical-deduped paths
// or the `len(tickets) != len(paths)` validation rejects the
// server's perfectly-valid response.
//
// Asserts:
//
//	(1) The RPC carries the deduped path list, not the raw one.
//	(2) First-occurrence shape survives (the operator wrote `dist`
//	    first, so `dist` reaches the server even though `dist/`
//	    came right after).
//	(3) The length check uses the deduped count, so a server that
//	    returns N tickets for N unique paths is accepted.
func TestUpload_DedupesPathsBeforeRPC(t *testing.T) {
	// Prep a workspace with the files the tar step needs.
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "dist"), 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "dist", "out"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dist/out: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "coverage.out"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write coverage: %v", err)
	}

	// Stub PUT endpoint — accepts any body, returns 200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = r.Body.Read(make([]byte, 1024))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stub := &stubAgentClient{
		tickets: []*gocdnextv1.ArtifactUploadTicket{
			{Path: "dist", StorageKey: "k1", PutUrl: srv.URL + "/dist", ExpiresAt: timestamppb.Now()},
			{Path: "coverage.out", StorageKey: "k2", PutUrl: srv.URL + "/cov", ExpiresAt: timestamppb.Now()},
		},
	}
	u := NewArtifactUploader(stub, "sess", srv.Client())

	// Duplicate `dist` in three different shapes; coverage.out unique.
	refs, err := u.Upload(context.Background(), workDir, "run-1", "job-1",
		[]string{"dist", "dist/", "dist", "coverage.out"})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2 (dedupe should collapse the dist variants)", len(refs))
	}

	if stub.lastRequest == nil {
		t.Fatal("stub never received the RPC")
	}
	got := stub.lastRequest.GetPaths()
	if len(got) != 2 {
		t.Fatalf("RPC paths = %d, want 2 (deduped before RPC): %v", len(got), got)
	}
	if got[0] != "dist" {
		t.Errorf("RPC paths[0] = %q, want %q (first-occurrence shape)", got[0], "dist")
	}
	if got[1] != "coverage.out" {
		t.Errorf("RPC paths[1] = %q, want %q", got[1], "coverage.out")
	}
}

// --- #16: glob expansion in UploadFromPod (isolated mode) -------------

// fakeArtifactExec is the rpc-test stub for engine.PodExecutor.
// Distinct from the runner-package routingFakeExec because the
// packages can't share private types and the upload tests don't
// need the same surface (no cat support).
type fakeArtifactExec struct {
	mu        sync.Mutex
	findOut   string                  // newline-separated absolute paths
	findErr   error                   // returned from `find` exec
	tarBodies map[string]string       // path → response body the tar would emit
	gotTars   [][]string              // recorded tar invocations (full argv)
	gotFinds  int
	allCalls  []string                // cmd[0] sequence for assertion
}

var _ engine.PodExecutor = (*fakeArtifactExec)(nil)

func (f *fakeArtifactExec) Exec(_ context.Context, _, _ string, cmd []string,
	_ io.Reader, stdout, stderr io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(cmd) == 0 {
		return errors.New("empty command")
	}
	f.allCalls = append(f.allCalls, cmd[0])
	switch cmd[0] {
	case "find":
		f.gotFinds++
		if f.findErr != nil {
			_, _ = stderr.Write([]byte("find failed"))
			return f.findErr
		}
		_, _ = stdout.Write([]byte(f.findOut))
		return nil
	case "tar":
		f.gotTars = append(f.gotTars, append([]string(nil), cmd...))
		// Emit any valid bytes so the size+sha calculation downstream
		// doesn't trip on empty input — the actual content is opaque
		// to this test (the PUT endpoint accepts whatever it sees).
		_, _ = stdout.Write([]byte("fake-tar-content"))
		return nil
	default:
		return errors.New("unexpected command: " + cmd[0])
	}
}

// TestUploadFromPod_ExpandsGlobsBeforeTar — the issue #16 regression
// cover. Operator declares `dist/*.tar.gz`; the agent must NOT pass
// the literal `*` to tar (tar would look for a file named `*.tar.gz`,
// not expand the glob — the housekeeper exec runs tar directly, no
// shell). Instead, agent enumerates via `find`, doublestar-matches
// against the declared pattern, and feeds the resolved relative
// paths to tar.
func TestUploadFromPod_ExpandsGlobsBeforeTar(t *testing.T) {
	const workDir = "/workspace/src/abc"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exec := &fakeArtifactExec{
		findOut: strings.Join([]string{
			workDir + "/dist/a.tar.gz",
			workDir + "/dist/b.tar.gz",
			workDir + "/build.gradle", // shouldn't match the glob
		}, "\n") + "\n",
	}
	stub := &stubAgentClient{
		tickets: []*gocdnextv1.ArtifactUploadTicket{
			{Path: "dist/*.tar.gz", StorageKey: "k1", PutUrl: srv.URL + "/x", ExpiresAt: timestamppb.Now()},
		},
	}
	u := NewArtifactUploader(stub, "sess", srv.Client())

	refs, err := u.UploadFromPod(context.Background(), exec,
		"pod-X", "housekeeper", workDir, "run-1", "job-1",
		[]string{"dist/*.tar.gz"})
	if err != nil {
		t.Fatalf("UploadFromPod: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(refs))
	}
	if refs[0].GetPath() != "dist/*.tar.gz" {
		t.Errorf("ref.path = %q, want operator-typed form", refs[0].GetPath())
	}

	// Find happened exactly once (shared across all declared paths
	// in a future multi-path call).
	if exec.gotFinds != 1 {
		t.Errorf("find calls = %d, want 1", exec.gotFinds)
	}

	// Exactly one tar invocation, carrying BOTH resolved files as
	// separate arguments AFTER the `--` separator. NOT the literal
	// glob.
	if len(exec.gotTars) != 1 {
		t.Fatalf("tar calls = %d, want 1; %v", len(exec.gotTars), exec.gotTars)
	}
	tarCmd := exec.gotTars[0]
	// tarCmd = ["tar", "-czf", "-", "-C", workDir, "--", "dist/a.tar.gz", "dist/b.tar.gz"]
	if len(tarCmd) < 8 {
		t.Fatalf("tar argv too short: %v", tarCmd)
	}
	if tarCmd[5] != "--" {
		t.Errorf("`--` separator missing at argv[5]: %v", tarCmd)
	}
	tarFiles := tarCmd[6:]
	if !sliceContains(tarFiles, "dist/a.tar.gz") || !sliceContains(tarFiles, "dist/b.tar.gz") {
		t.Errorf("tar argv files = %v, want both resolved matches", tarFiles)
	}
	for _, f := range tarFiles {
		if strings.Contains(f, "*") {
			t.Errorf("tar received unexpanded glob %q — this is exactly the bug we fixed", f)
		}
	}
}

// TestUploadFromPod_LiteralPathSkipsFind — the fast-path: declared
// path with no glob characters bypasses the `find` exec entirely.
// Saves an exec round-trip on the common case (most jobs declare
// concrete artifacts like `main/build/libs/app.jar`, not globs).
func TestUploadFromPod_LiteralPathSkipsFind(t *testing.T) {
	const workDir = "/workspace"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exec := &fakeArtifactExec{} // find unused; tar gets the literal
	stub := &stubAgentClient{
		tickets: []*gocdnextv1.ArtifactUploadTicket{
			{Path: "build/libs/app.jar", StorageKey: "k1", PutUrl: srv.URL, ExpiresAt: timestamppb.Now()},
		},
	}
	u := NewArtifactUploader(stub, "sess", srv.Client())

	_, err := u.UploadFromPod(context.Background(), exec,
		"pod-X", "housekeeper", workDir, "run-1", "job-1",
		[]string{"build/libs/app.jar"})
	if err != nil {
		t.Fatalf("UploadFromPod: %v", err)
	}

	if exec.gotFinds != 0 {
		t.Errorf("find calls = %d, want 0 for literal path (fast-path miss)", exec.gotFinds)
	}
	if len(exec.gotTars) != 1 {
		t.Fatalf("tar calls = %d, want 1", len(exec.gotTars))
	}
	last := exec.gotTars[0]
	if last[len(last)-1] != "build/libs/app.jar" {
		t.Errorf("tar received %q, want literal declared path", last[len(last)-1])
	}
}

// TestUploadFromPod_ZeroMatchesErrorsLoud — operator declared a
// glob that resolves to zero files. Pre-honesty-fix this returned
// (nil, nil) and PostJob's required branch silently succeeded
// with no artefact — a REGRESSION vs the shared-mode posture
// where a missing literal would surface as an os.Stat error and
// fail the job. Returning a typed error lets PostJob bubble it on
// the required path while still swallowing on optional.
func TestUploadFromPod_ZeroMatchesErrorsLoud(t *testing.T) {
	const workDir = "/workspace"
	exec := &fakeArtifactExec{findOut: workDir + "/main.go\n"} // no .xml files
	stub := &stubAgentClient{}                                 // tickets unused — the test asserts the RPC isn't made
	u := NewArtifactUploader(stub, "sess", &http.Client{})

	refs, err := u.UploadFromPod(context.Background(), exec,
		"pod-X", "housekeeper", workDir, "run-1", "job-1",
		[]string{"build/reports/**/*.xml"})
	if err == nil {
		t.Fatal("want error for zero-match glob, got nil (this is the silent-success regression)")
	}
	var pathsErr *ArtifactPathsMissingError
	if !errors.As(err, &pathsErr) {
		t.Errorf("want *ArtifactPathsMissingError, got %T: %v", err, err)
	} else if len(pathsErr.Paths) != 1 || pathsErr.Paths[0] != "build/reports/**/*.xml" {
		t.Errorf("missing paths = %v, want [build/reports/**/*.xml]", pathsErr.Paths)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0 (nothing uploaded)", len(refs))
	}
	if stub.lastRequest != nil {
		t.Errorf("RPC fired despite zero matches: %+v", stub.lastRequest)
	}
}

// TestUploadFromPod_PartialMatchStillBubblesErrorForMissing — when
// one declared path matches and another doesn't, the matching one
// stays in flight but the error still names the missing path.
// PostJob's required branch surfaces this; optional swallows.
func TestUploadFromPod_PartialMatchStillBubblesErrorForMissing(t *testing.T) {
	const workDir = "/workspace"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exec := &fakeArtifactExec{
		findOut: workDir + "/dist/app.jar\n",
	}
	stub := &stubAgentClient{}
	u := NewArtifactUploader(stub, "sess", srv.Client())

	_, err := u.UploadFromPod(context.Background(), exec,
		"pod-X", "housekeeper", workDir, "run-1", "job-1",
		[]string{"dist/*.jar", "build/reports/**/*.xml"})
	if err == nil {
		t.Fatal("want error for one zero-match path among two declared, got nil")
	}
	var pathsErr *ArtifactPathsMissingError
	if !errors.As(err, &pathsErr) {
		t.Fatalf("want *ArtifactPathsMissingError, got %T: %v", err, err)
	}
	if len(pathsErr.Paths) != 1 || pathsErr.Paths[0] != "build/reports/**/*.xml" {
		t.Errorf("missing paths = %v, want only the unmatched one", pathsErr.Paths)
	}
}

// TestUpload_TicketCountMismatchStillSurfacesAsError — sanity check
// that a misbehaving server returning a wrong count (NOT just a
// deduped count) still fails the upload loudly. The validation
// uses len(unique), so a server returning 1 ticket for 2 unique
// paths must be caught.
func TestUpload_TicketCountMismatchStillSurfacesAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stub := &stubAgentClient{
		tickets: []*gocdnextv1.ArtifactUploadTicket{
			{Path: "dist", StorageKey: "k1", PutUrl: srv.URL, ExpiresAt: timestamppb.Now()},
			// Missing ticket for `other` — server bug.
		},
	}
	u := NewArtifactUploader(stub, "sess", srv.Client())
	_, err := u.Upload(context.Background(), t.TempDir(), "run-1", "job-1",
		[]string{"dist", "other"})
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "1 tickets for 2 paths") {
		t.Fatalf("err = %v, want '1 tickets for 2 paths'", err)
	}
}
