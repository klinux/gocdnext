package rpc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// stubCacheClient implements just enough of
// gocdnextv1.AgentServiceClient for cache fetch+store round-trips.
type stubCacheClient struct {
	gocdnextv1.AgentServiceClient

	getResp    *gocdnextv1.RequestCacheGetResponse
	getErr     error
	getCalls   []*gocdnextv1.RequestCacheGetRequest
	putResp    *gocdnextv1.RequestCachePutResponse
	putErr     error
	putCalls   []*gocdnextv1.RequestCachePutRequest
	readyCalls []*gocdnextv1.MarkCacheReadyRequest
	readyResp  *gocdnextv1.MarkCacheReadyResponse
	readyErr   error
}

func (s *stubCacheClient) RequestCacheGet(_ context.Context, in *gocdnextv1.RequestCacheGetRequest, _ ...grpc.CallOption) (*gocdnextv1.RequestCacheGetResponse, error) {
	s.getCalls = append(s.getCalls, in)
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.getResp, nil
}

func (s *stubCacheClient) RequestCachePut(_ context.Context, in *gocdnextv1.RequestCachePutRequest, _ ...grpc.CallOption) (*gocdnextv1.RequestCachePutResponse, error) {
	s.putCalls = append(s.putCalls, in)
	if s.putErr != nil {
		return nil, s.putErr
	}
	return s.putResp, nil
}

func (s *stubCacheClient) MarkCacheReady(_ context.Context, in *gocdnextv1.MarkCacheReadyRequest, _ ...grpc.CallOption) (*gocdnextv1.MarkCacheReadyResponse, error) {
	s.readyCalls = append(s.readyCalls, in)
	if s.readyErr != nil {
		return nil, s.readyErr
	}
	return s.readyResp, nil
}

// recordingExecutor captures the cmd argv passed to Exec and
// optionally writes a fixed payload to stdout (simulating tar
// output) so the surrounding pipeline (tmp file → PUT) round-trips.
type recordingExecutor struct {
	mu          sync.Mutex
	lastCmd     []string
	stdoutBytes []byte
	err         error
}

func (r *recordingExecutor) Exec(_ context.Context, _, _ string, cmd []string, _ io.Reader, stdout, _ io.Writer) error {
	r.mu.Lock()
	r.lastCmd = append([]string(nil), cmd...)
	r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if stdout != nil && len(r.stdoutBytes) > 0 {
		_, _ = stdout.Write(r.stdoutBytes)
	}
	return nil
}

func TestResolveGet_FoundReturnsTicket(t *testing.T) {
	stub := &stubCacheClient{
		getResp: &gocdnextv1.RequestCacheGetResponse{
			Found:         true,
			GetUrl:        "https://signed.example/get",
			ContentSha256: "deadbeef",
		},
	}
	c := NewCacheClient(stub, "sess-1", nil)
	url, sha, found, err := c.ResolveGet(context.Background(), "run-1", "job-1", "trivy-db")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !found {
		t.Errorf("found: want true")
	}
	if url != "https://signed.example/get" {
		t.Errorf("url: got %q", url)
	}
	if sha != "deadbeef" {
		t.Errorf("sha: got %q", sha)
	}
	if got := len(stub.getCalls); got != 1 {
		t.Fatalf("RequestCacheGet calls: want 1, got %d", got)
	}
	if stub.getCalls[0].GetSessionId() != "sess-1" {
		t.Errorf("session id not propagated")
	}
	if stub.getCalls[0].GetKey() != "trivy-db" {
		t.Errorf("key not propagated")
	}
}

func TestResolveGet_NotFoundIsNoError(t *testing.T) {
	stub := &stubCacheClient{
		getResp: &gocdnextv1.RequestCacheGetResponse{Found: false},
	}
	c := NewCacheClient(stub, "s", nil)
	_, _, found, err := c.ResolveGet(context.Background(), "r", "j", "k")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if found {
		t.Errorf("found: want false")
	}
}

func TestResolveGet_NotFoundCodeIsNoError(t *testing.T) {
	// Legacy server: NotFound is the wire-level "miss" signal.
	stub := &stubCacheClient{
		getErr: status.Error(codes.NotFound, "no ready row"),
	}
	c := NewCacheClient(stub, "s", nil)
	_, _, found, err := c.ResolveGet(context.Background(), "r", "j", "k")
	if err != nil {
		t.Fatalf("NotFound should normalise to found=false, got err %v", err)
	}
	if found {
		t.Errorf("found: want false")
	}
}

func TestStoreFromPod_FiltersMissingPaths(t *testing.T) {
	// The tar command issued inside the housekeeper must filter
	// missing paths the way shared mode's TarGzPaths does — without
	// this, a single missing entry made the whole cache fail to
	// upload. The shell wrapper does the filtering via tar -T <file>,
	// so the cmd argv is `sh -c <script> _ <workdir> path1 path2 ...`
	// (NOT a raw `tar -czf - -- path1 path2`).
	exec := &recordingExecutor{
		// Empty tar.gz envelope so the downstream PUT has something
		// to send (~30 bytes).
		stdoutBytes: []byte("\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\x03\x03\x00\x00\x00\x00\x00\x00\x00\x00\x00"),
	}

	put := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer put.Close()

	stub := &stubCacheClient{
		putResp: &gocdnextv1.RequestCachePutResponse{
			CacheId: "cache-1",
			PutUrl:  put.URL,
		},
		readyResp: &gocdnextv1.MarkCacheReadyResponse{},
	}
	c := NewCacheClient(stub, "s", nil)

	err := c.StoreFromPod(context.Background(), exec,
		"pod-1", "housekeeper", "/workspace",
		"run-1", "job-1",
		&gocdnextv1.CacheEntry{
			Key:   "trivy-db",
			Paths: []string{".trivy-cache", "missing-dir"},
		})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	exec.mu.Lock()
	cmd := exec.lastCmd
	exec.mu.Unlock()

	// Argv shape: sh -c <script> _ <workdir> <path...>
	if len(cmd) < 5 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("expected sh -c wrapper, got %v", cmd)
	}
	if cmd[4] != "/workspace" {
		t.Errorf("workdir arg: want /workspace, got %q", cmd[4])
	}
	// Both declared paths reach the wrapper as positional args; the
	// filter runs INSIDE the shell. Caller doesn't pre-filter.
	if !sliceContains(cmd[5:], ".trivy-cache") || !sliceContains(cmd[5:], "missing-dir") {
		t.Errorf("paths argv: got %v", cmd[5:])
	}
	// Critical: the script body must filter via `[ -e "$p" ]` so
	// missing paths don't blow up tar.
	if !strings.Contains(cmd[2], "[ -e") {
		t.Errorf("script body missing existence filter:\n%s", cmd[2])
	}
	// And feed tar via -T <file> so the surviving list is space-safe.
	if !strings.Contains(cmd[2], "tar -czf - -T") {
		t.Errorf("script body missing tar -T pipe:\n%s", cmd[2])
	}

	if got := len(stub.putCalls); got != 1 {
		t.Fatalf("RequestCachePut calls: want 1, got %d", got)
	}
	if got := len(stub.readyCalls); got != 1 {
		t.Fatalf("MarkCacheReady calls: want 1, got %d", got)
	}
	if stub.readyCalls[0].GetCacheId() != "cache-1" {
		t.Errorf("ready cache id: got %q", stub.readyCalls[0].GetCacheId())
	}
}

func TestStoreFromPod_EmptyPathsRejected(t *testing.T) {
	c := NewCacheClient(&stubCacheClient{}, "s", nil)
	err := c.StoreFromPod(context.Background(), &recordingExecutor{},
		"p", "hk", "/workspace", "r", "j",
		&gocdnextv1.CacheEntry{Key: "k", Paths: nil})
	if err == nil {
		t.Fatal("expected error on empty paths")
	}
}

func TestStoreFromPod_NilExecutorRejected(t *testing.T) {
	c := NewCacheClient(&stubCacheClient{}, "s", nil)
	err := c.StoreFromPod(context.Background(), nil,
		"p", "hk", "/workspace", "r", "j",
		&gocdnextv1.CacheEntry{Key: "k", Paths: []string{"x"}})
	if err == nil {
		t.Fatal("expected error on nil executor")
	}
}

func sliceContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
