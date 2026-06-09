package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/runner"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// CacheClient implements runner.CacheClient. It drives the three
// cache RPCs (RequestCacheGet, RequestCachePut, MarkCacheReady)
// and the signed HTTP transfers that bracket them.
//
// Cache is best-effort: every error returned here is a signal the
// runner uses to log and move on. A clean NotFound from Get comes
// back as found=false without an error (that's the cold-start
// case and the whole point of the design). A transport or
// protocol error — bad URL, sha mismatch, MarkReady refusing —
// comes back as an error so the log makes the failure visible,
// but the runner still treats the job as successful if
// everything else was.
type CacheClient struct {
	client    gocdnextv1.AgentServiceClient
	sessionID string
	http      *http.Client
}

// NewCacheClient wires the concrete cache client. Shares the same
// http.Client shape as the artifact uploader — nil means "30-min
// timeout" which is generous on purpose (agent cold-pulling a
// 2 GB pnpm-store over a slow S3 bucket is a real scenario).
func NewCacheClient(client gocdnextv1.AgentServiceClient, sessionID string, httpClient *http.Client) *CacheClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Minute}
	}
	return &CacheClient{client: client, sessionID: sessionID, http: httpClient}
}

// Fetch implements runner.CacheClient.Fetch.
func (c *CacheClient) Fetch(ctx context.Context, workDir, runID, jobID string, entry *gocdnextv1.CacheEntry) (bool, error) {
	resp, err := c.client.RequestCacheGet(ctx, &gocdnextv1.RequestCacheGetRequest{
		SessionId: c.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Key:       entry.GetKey(),
	})
	if err != nil {
		// NotFound from the server means "no ready row yet" —
		// match the Found=false semantics so the runner handles
		// cold start the same whether the server returned miss
		// via found=false OR via a legacy NotFound code.
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
			return false, nil
		}
		return false, fmt.Errorf("request cache get: %w", err)
	}
	if !resp.GetFound() {
		return false, nil
	}
	if err := runner.DownloadAndUntar(ctx, c.http, resp.GetGetUrl(), workDir, resp.GetContentSha256()); err != nil {
		return false, fmt.Errorf("download+untar: %w", err)
	}
	return true, nil
}

// ResolveGet calls RequestCacheGet and returns the signed URL
// + sha + found flag WITHOUT downloading. Used by the isolated
// workspace runner to pre-populate CacheEntry's fetch_url at
// dispatch time so the init container can fetch via HTTP
// without holding a gRPC session.
//
// NotFound from the server normalises to found=false (matches
// Fetch's semantics).
func (c *CacheClient) ResolveGet(ctx context.Context, runID, jobID, key string) (url, sha string, found bool, err error) {
	resp, rpcErr := c.client.RequestCacheGet(ctx, &gocdnextv1.RequestCacheGetRequest{
		SessionId: c.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Key:       key,
	})
	if rpcErr != nil {
		if st, ok := status.FromError(rpcErr); ok && st.Code() == codes.NotFound {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("request cache get: %w", rpcErr)
	}
	if !resp.GetFound() {
		return "", "", false, nil
	}
	return resp.GetGetUrl(), resp.GetContentSha256(), true, nil
}

// StoreFromPod is the isolated-mode counterpart of Store: the
// tar source is inside the job pod's housekeeper sidecar
// (streamed via PodExecutor + a local temp file to derive
// Content-Length) instead of the agent's local workDir. Same
// gRPC RequestCachePut → PUT → MarkCacheReady dance, except a
// pre-flight probe inside the pod is run first to filter out
// missing paths the way shared-mode TarGzPaths does.
//
// If the probe shows that NO declared path actually exists
// (cold start, conditionally-generated output that wasn't
// produced this run, …), the entire RPC sequence is skipped —
// RequestCachePut + MarkCacheReady on a 0-byte PUT would poison
// the cache key (downstream Fetch would gzip-parse-fail). This
// differs from shared mode, which still uploads a ~23-byte valid
// empty tar.gz; the isolated path errs on the side of "don't
// touch the row" because empty-ready entries are operationally
// noisier than no entry at all.
//
// Best-effort like Store: callers log on error and continue.
func (c *CacheClient) StoreFromPod(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, containerName, podWorkDir string,
	runID, jobID string,
	entry *gocdnextv1.CacheEntry,
) error {
	if len(entry.GetPaths()) == 0 {
		return errors.New("cache: entry has no paths")
	}
	if exec == nil {
		return errors.New("cache: nil executor")
	}

	// 1. Probe — list existing paths (defanged for leading-dash)
	//    INSIDE the pod. Single round-trip per cache entry.
	existing, err := c.probeCachePaths(ctx, exec, podName, containerName, podWorkDir, entry.GetPaths())
	if err != nil {
		return fmt.Errorf("probe paths: %w", err)
	}
	if len(existing) == 0 {
		// Mirror shared-mode semantics: TarGzPaths(workDir, nil)
		// produces a valid empty tar.gz (~23 bytes), which Store
		// then PUTs and marks ready. The cache row gets a fresh
		// empty blob, NOT preserved as whatever older entry was
		// there. Skipping the whole RPC sequence (as v0.5.6 did)
		// would silently keep a stale ready blob from an earlier
		// run when the job DID produce the path — a divergence
		// from shared mode that surprises operators.
		return c.storeEmptyCacheBlob(ctx, runID, jobID, entry.GetKey())
	}

	// 2. RPC + tar + PUT + ready — only when there's content.
	put, err := c.client.RequestCachePut(ctx, &gocdnextv1.RequestCachePutRequest{
		SessionId: c.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Key:       entry.GetKey(),
	})
	if err != nil {
		return fmt.Errorf("request cache put: %w", err)
	}

	tmp, err := os.CreateTemp("", "gocdnext-cache-pod-*.tar.gz")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)

	// Tar the already-filtered list. The paths reach the shell
	// as positional args; the wrapper writes them to a tempfile
	// (preserves spaces) and feeds `tar -T <file>`. Paths
	// starting with `-` were defanged to `./-foo` by the probe,
	// so tar can't read them as options. No need to re-filter
	// existence — probe already did it.
	const tarScript = `cd "$1" || exit 1; shift; ` +
		`tmp=$(mktemp) || exit 1; trap "rm -f $tmp" EXIT; ` +
		`for p in "$@"; do printf '%s\n' "$p" >> "$tmp"; done; ` +
		`exec tar -czf - -T "$tmp"`
	cmd := append([]string{"sh", "-c", tarScript, "_", podWorkDir}, existing...)
	if err := exec.Exec(ctx, podName, containerName, cmd, nil, mw, io.Discard); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("exec tar %q: %w", entry.GetKey(), err)
	}

	info, statErr := tmp.Stat()
	if cerr := tmp.Close(); cerr != nil && statErr == nil {
		statErr = cerr
	}
	if statErr != nil {
		return fmt.Errorf("stat tar tmp: %w", statErr)
	}
	size := info.Size()

	body, err := os.Open(tmpName)
	if err != nil {
		return fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = body.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, put.GetPutUrl(), body)
	if err != nil {
		return fmt.Errorf("build PUT: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http PUT: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT returned %s", resp.Status)
	}

	if _, err := c.client.MarkCacheReady(ctx, &gocdnextv1.MarkCacheReadyRequest{
		SessionId:     c.sessionID,
		CacheId:       put.GetCacheId(),
		SizeBytes:     size,
		ContentSha256: hex.EncodeToString(hasher.Sum(nil)),
	}); err != nil {
		return fmt.Errorf("mark cache ready: %w", err)
	}
	return nil
}

// storeEmptyCacheBlob uploads a valid-but-empty tar.gz under
// the given key. Used in the "all paths missing" branch so the
// cache row gets refreshed rather than left pointing at stale
// content from a previous run — same behaviour as shared mode's
// TarGzPaths, which writes a header + EOF marker (~23 bytes)
// when no paths exist.
//
// runner.TarGzPaths is reused for the encoding so any sha/size
// drift between the empty and non-empty paths stays impossible.
func (c *CacheClient) storeEmptyCacheBlob(ctx context.Context, runID, jobID, key string) error {
	var buf bytes.Buffer
	sha, size, err := runner.TarGzPaths("", nil, &buf)
	if err != nil {
		return fmt.Errorf("empty tar: %w", err)
	}

	put, err := c.client.RequestCachePut(ctx, &gocdnextv1.RequestCachePutRequest{
		SessionId: c.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Key:       key,
	})
	if err != nil {
		return fmt.Errorf("request cache put: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, put.GetPutUrl(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("build PUT: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http PUT: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT returned %s", resp.Status)
	}

	if _, err := c.client.MarkCacheReady(ctx, &gocdnextv1.MarkCacheReadyRequest{
		SessionId:     c.sessionID,
		CacheId:       put.GetCacheId(),
		SizeBytes:     size,
		ContentSha256: sha,
	}); err != nil {
		return fmt.Errorf("mark cache ready: %w", err)
	}
	return nil
}

// probeCachePaths runs a quick `[ -e ]` test inside the pod for
// every declared path and returns the survivors. Paths beginning
// with `-` are prefixed with `./` (defang) so neither the
// existence test NOR the eventual tar invocation can misread
// them as options on the platforms whose `[`/`tar` implementations
// flirt with that ambiguity.
//
// Single exec per cache entry; trades one ~100ms round-trip for
// cacheProbeScript returns the shell script the cache STORE path
// execs inside the housekeeper to list which of the declared paths
// actually exist post-task. Extracted to a helper so a test can
// drive it via `os/exec` against a real `sh` and pin the contract
// that "no paths match" is exit 0 + empty stdout, NOT exit 1.
//
// Trailing `exit 0` is load-bearing: shell exit follows the LAST
// command executed, which is `[ -e "$p" ]` inside the loop. If
// NONE of the declared paths exist (cache-miss-first-run scenario
// — operator added a cache block, Gradle/pnpm hasn't populated
// `.gradle-home/`/`node_modules/` yet), every test returns 1 and
// the script would exit 1 without the trailer. The caller then
// wraps it as `cache store failed (probe paths: exit 1)` —
// alarming noise for what is functionally "no paths to tar, store
// empty". Forcing exit 0 keeps the script honest: stdout carries
// the path list (possibly empty), exit code reflects "the probe
// ran cleanly", and the caller routes via the empty-list branch
// (storeEmptyCacheBlob) without a fake error.
func cacheProbeScript() string {
	return `cd "$1" || exit 1; shift; ` +
		`for p in "$@"; do ` +
		`  case "$p" in -*) p="./$p" ;; esac; ` +
		`  [ -e "$p" ] && printf '%s\n' "$p"; ` +
		`done; exit 0`
}

// the ability to skip the whole RequestCachePut + PUT +
// MarkCacheReady sequence when nothing exists.
func (c *CacheClient) probeCachePaths(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, containerName, podWorkDir string,
	paths []string,
) ([]string, error) {
	probeScript := cacheProbeScript()
	cmd := append([]string{"sh", "-c", probeScript, "_", podWorkDir}, paths...)
	var out bytes.Buffer
	if err := exec.Exec(ctx, podName, containerName, cmd, nil, &out, io.Discard); err != nil {
		return nil, err
	}
	lines := strings.Split(out.String(), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result, nil
}

// Store implements runner.CacheClient.Store.
func (c *CacheClient) Store(ctx context.Context, workDir, runID, jobID string, entry *gocdnextv1.CacheEntry) error {
	if len(entry.GetPaths()) == 0 {
		// A key with no paths has no tarball to upload. The parser
		// already rejects this shape at pipeline apply time, but
		// guard here too — defence in depth against a future
		// assignment builder that forgets to copy paths through.
		return errors.New("cache: entry has no paths")
	}

	put, err := c.client.RequestCachePut(ctx, &gocdnextv1.RequestCachePutRequest{
		SessionId: c.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Key:       entry.GetKey(),
	})
	if err != nil {
		return fmt.Errorf("request cache put: %w", err)
	}

	sha, size, err := runner.TarAndUpload(ctx, c.http, put.GetPutUrl(), workDir, entry.GetPaths())
	if err != nil {
		return fmt.Errorf("tar+upload: %w", err)
	}

	if _, err := c.client.MarkCacheReady(ctx, &gocdnextv1.MarkCacheReadyRequest{
		SessionId:     c.sessionID,
		CacheId:       put.GetCacheId(),
		SizeBytes:     size,
		ContentSha256: sha,
	}); err != nil {
		return fmt.Errorf("mark cache ready: %w", err)
	}
	return nil
}
