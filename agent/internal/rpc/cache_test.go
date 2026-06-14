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

// recordingExecutor captures every cmd argv passed to Exec and
// emits a per-call stdout payload (a fake of what the in-pod
// process would have written). Used to exercise StoreFromPod's
// two-exec dance (probe → tar) without a real cluster: we drive
// the probe response and assert the tar invocation that follows.
type recordingExecutor struct {
	mu      sync.Mutex
	cmds    [][]string
	stdouts [][]byte // index N is the payload for the N-th Exec call
	errs    []error  // index N is the error for the N-th call (nil ok)
}

func (r *recordingExecutor) Exec(_ context.Context, _, _ string, cmd []string, _ io.Reader, stdout, _ io.Writer) error {
	r.mu.Lock()
	idx := len(r.cmds)
	r.cmds = append(r.cmds, append([]string(nil), cmd...))
	var payload []byte
	if idx < len(r.stdouts) {
		payload = r.stdouts[idx]
	}
	var perr error
	if idx < len(r.errs) {
		perr = r.errs[idx]
	}
	r.mu.Unlock()
	if perr != nil {
		return perr
	}
	if stdout != nil && len(payload) > 0 {
		_, _ = stdout.Write(payload)
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

// emptyTarGz is the literal bytes of an empty (zero-entry)
// gzipped tar archive — what a real `tar -czf - -T <empty>`
// would have produced. Used to drive the test PUT pipeline so
// the tmp file has a valid Content-Length without depending on
// a real tar binary.
var emptyTarGz = []byte("\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\x03\x03\x00\x00\x00\x00\x00\x00\x00\x00\x00")

func TestStoreFromPod_HappyPath_TwoExecsAndFullRPC(t *testing.T) {
	// Probe emits 2 existing paths; the second exec (tar) then
	// runs against that filtered list. We assert:
	//   - probe argv shape (sh -c <probe-script> _ <workdir> <paths>)
	//   - tar argv shape (sh -c <tar-script> _ <workdir> <existing>)
	//   - RequestCachePut + PUT + MarkCacheReady all fire once
	exec := &recordingExecutor{
		stdouts: [][]byte{
			[]byte(".trivy-cache\nnode_modules\n"), // probe stdout
			emptyTarGz,                             // tar stdout
		},
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
	c := NewCacheClient(stub, "sess", nil)

	_, err := c.StoreFromPod(context.Background(), exec,
		"pod-1", "housekeeper", "/workspace",
		"run-1", "job-1",
		&gocdnextv1.CacheEntry{
			Key:   "trivy-db",
			Paths: []string{".trivy-cache", "node_modules", "missing-dir"},
		})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	exec.mu.Lock()
	calls := append([][]string(nil), exec.cmds...)
	exec.mu.Unlock()

	if got := len(calls); got != 2 {
		t.Fatalf("want 2 exec calls (probe + tar), got %d: %v", got, calls)
	}

	probe := calls[0]
	if probe[0] != "sh" || probe[1] != "-c" {
		t.Fatalf("probe shape: want sh -c, got %v", probe)
	}
	if probe[4] != "/workspace" {
		t.Errorf("probe workdir: got %q", probe[4])
	}
	// All declared paths (including the missing one) reach the
	// probe so it can do the filtering in-pod.
	if !sliceContains(probe[5:], ".trivy-cache") ||
		!sliceContains(probe[5:], "node_modules") ||
		!sliceContains(probe[5:], "missing-dir") {
		t.Errorf("probe paths argv: got %v", probe[5:])
	}
	// Probe script must:
	//   (a) defang leading-dash paths (`case "$p" in -*) p="./$p" ;;`)
	//   (b) test existence (`[ -e "$p" ]`)
	if !strings.Contains(probe[2], `case "$p" in -*)`) {
		t.Errorf("probe missing leading-dash defang:\n%s", probe[2])
	}
	if !strings.Contains(probe[2], "[ -e") {
		t.Errorf("probe missing existence test:\n%s", probe[2])
	}

	tarCmd := calls[1]
	if tarCmd[0] != "sh" || tarCmd[1] != "-c" {
		t.Fatalf("tar shape: want sh -c, got %v", tarCmd)
	}
	// Only the survivors from probe stdout reach the tar exec —
	// the missing entry was filtered out agent-side.
	if !sliceContains(tarCmd[5:], ".trivy-cache") ||
		!sliceContains(tarCmd[5:], "node_modules") {
		t.Errorf("tar should receive survivors, got %v", tarCmd[5:])
	}
	if sliceContains(tarCmd[5:], "missing-dir") {
		t.Errorf("tar should NOT receive missing path, got %v", tarCmd[5:])
	}
	if !strings.Contains(tarCmd[2], "tar -czf - -T") {
		t.Errorf("tar script missing -T pipe:\n%s", tarCmd[2])
	}

	if got := len(stub.putCalls); got != 1 {
		t.Errorf("RequestCachePut calls: want 1, got %d", got)
	}
	if got := len(stub.readyCalls); got != 1 {
		t.Errorf("MarkCacheReady calls: want 1, got %d", got)
	}
}

func TestStoreFromPod_AllPathsMissing_UploadsValidEmptyTarGz(t *testing.T) {
	// Mirror shared-mode TarGzPaths: when no path exists, write
	// a valid empty tar.gz and call RequestCachePut +
	// MarkCacheReady on it. This REFRESHES the cache row to
	// empty rather than leaving a stale ready blob from an
	// earlier run that did produce content.
	//
	// Only one exec runs (the probe) — the empty-blob path
	// builds the tar bytes agent-side, so the tar exec is
	// skipped entirely.
	exec := &recordingExecutor{
		stdouts: [][]byte{[]byte("")}, // probe finds nothing
	}

	var putReceivedLen int64
	put := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		putReceivedLen = r.ContentLength
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer put.Close()

	stub := &stubCacheClient{
		putResp: &gocdnextv1.RequestCachePutResponse{
			CacheId: "cache-empty",
			PutUrl:  put.URL,
		},
		readyResp: &gocdnextv1.MarkCacheReadyResponse{},
	}
	c := NewCacheClient(stub, "sess", nil)

	_, err := c.StoreFromPod(context.Background(), exec,
		"pod-1", "housekeeper", "/workspace",
		"run-1", "job-1",
		&gocdnextv1.CacheEntry{
			Key:   "trivy-db",
			Paths: []string{"missing-a", "missing-b"},
		})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	exec.mu.Lock()
	calls := len(exec.cmds)
	exec.mu.Unlock()
	if calls != 1 {
		t.Errorf("want only probe exec (no tar), got %d calls", calls)
	}
	if len(stub.putCalls) != 1 {
		t.Errorf("RequestCachePut must fire to refresh the row, got %d", len(stub.putCalls))
	}
	if len(stub.readyCalls) != 1 {
		t.Errorf("MarkCacheReady must fire, got %d", len(stub.readyCalls))
	}
	if putReceivedLen <= 0 {
		t.Errorf("PUT must carry a non-empty body (empty tar.gz envelope), got Content-Length=%d", putReceivedLen)
	}
	if stub.readyCalls[0].GetContentSha256() == "" {
		t.Errorf("MarkCacheReady must carry the empty-tar.gz sha, got empty")
	}
	if stub.readyCalls[0].GetSizeBytes() != putReceivedLen {
		t.Errorf("ready size %d disagrees with PUT length %d",
			stub.readyCalls[0].GetSizeBytes(), putReceivedLen)
	}
}

func TestStoreFromPod_DefangsLeadingDashPath(t *testing.T) {
	// Probe should emit `./-dist` (not `-dist`) so the next tar
	// invocation can't read it as an option. Caller declared
	// `-dist`; both exec calls receive the raw arg, but the
	// probe script must rewrite via `case "$p" in -*) p="./$p"`.
	// We simulate the probe rewrite by feeding stdout="./-dist\n".
	exec := &recordingExecutor{
		stdouts: [][]byte{
			[]byte("./-dist\n"),
			emptyTarGz,
		},
	}
	put := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer put.Close()
	stub := &stubCacheClient{
		putResp:   &gocdnextv1.RequestCachePutResponse{CacheId: "c", PutUrl: put.URL},
		readyResp: &gocdnextv1.MarkCacheReadyResponse{},
	}
	c := NewCacheClient(stub, "sess", nil)

	size, err := c.StoreFromPod(context.Background(), exec,
		"p", "hk", "/workspace", "r", "j",
		&gocdnextv1.CacheEntry{Key: "k", Paths: []string{"-dist"}})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// A real tarball was uploaded — the size must propagate back so
	// the runner can report it in the store log line.
	if size <= 0 {
		t.Errorf("StoreFromPod size = %d, want > 0", size)
	}

	exec.mu.Lock()
	tarCmd := exec.cmds[1]
	exec.mu.Unlock()
	// tar argv must carry the defanged form, not the raw `-dist`.
	if !sliceContains(tarCmd[5:], "./-dist") {
		t.Errorf("tar should receive defanged ./-dist, got %v", tarCmd[5:])
	}
	if sliceContains(tarCmd[5:], "-dist") {
		t.Errorf("tar must NOT receive raw -dist (option-ambiguous), got %v", tarCmd[5:])
	}
}

func TestStoreFromPod_EmptyPathsRejected(t *testing.T) {
	c := NewCacheClient(&stubCacheClient{}, "s", nil)
	_, err := c.StoreFromPod(context.Background(), &recordingExecutor{},
		"p", "hk", "/workspace", "r", "j",
		&gocdnextv1.CacheEntry{Key: "k", Paths: nil})
	if err == nil {
		t.Fatal("expected error on empty paths")
	}
}

func TestStoreFromPod_NilExecutorRejected(t *testing.T) {
	c := NewCacheClient(&stubCacheClient{}, "s", nil)
	_, err := c.StoreFromPod(context.Background(), nil,
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
