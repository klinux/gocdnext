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
	// compression is the STORE-side codec ("gzip" default, "zstd" opt-in
	// via GOCDNEXT_CACHE_COMPRESSION). Restore is always codec-agnostic
	// (it sniffs the blob), so this only decides how NEW blobs are
	// written — the writer half of the reader-first zstd rollout (#274).
	compression string
}

// Cache compression codecs. gzip is the historical default and the fail-safe
// fallback; zstd is opt-in and requires the housekeeper image to carry the
// `zstd` binary (the dedicated image does).
const (
	codecGzip = "gzip"
	codecZstd = "zstd"
)

// UseCompression sets the store codec from an operator string, falling back
// to gzip for empty/unknown values so a typo never selects a codec the pod
// can't run. Called once per (re)connect from the agent's env.
func (c *CacheClient) UseCompression(codec string) {
	if strings.EqualFold(strings.TrimSpace(codec), codecZstd) {
		c.compression = codecZstd
		return
	}
	c.compression = codecGzip
}

// cacheTarScript returns the shell the housekeeper execs to produce the cache
// tarball on stdout. gzip is byte-identical to the pre-zstd script; zstd pipes
// tar into `zstd -T0` under pipefail (so a decompressor/compressor failure on
// a truncated stream isn't masked by tar's exit 0). $1 = workDir; the
// remaining positional args are the already-probed paths, written to a
// tempfile fed to `tar -T` (preserves spaces; `-`-leading paths were defanged
// by the probe).
func cacheTarScript(codec string) string {
	const prefix = `cd "$1" || exit 1; shift; ` +
		`tmp=$(mktemp) || exit 1; trap "rm -f $tmp" EXIT; ` +
		`for p in "$@"; do printf '%s\n' "$p" >> "$tmp"; done; `
	if codec == codecZstd {
		return `set -o pipefail; ` + prefix + `tar -cf - -T "$tmp" | zstd -T0 -3 -c`
	}
	return prefix + `exec tar -czf - -T "$tmp"`
}

// cacheContentType is the Content-Type for the signed PUT, matched to the
// codec. S3/GCS don't act on it, but it keeps the stored object honest.
func cacheContentType(codec string) string {
	if codec == codecZstd {
		return "application/zstd"
	}
	return "application/gzip"
}

// NewCacheClient wires the concrete cache client. Shares the same
// http.Client shape as the artifact uploader — nil means "30-min
// timeout" which is generous on purpose (agent cold-pulling a
// 2 GB pnpm-store over a slow S3 bucket is a real scenario).
func NewCacheClient(client gocdnextv1.AgentServiceClient, sessionID string, httpClient *http.Client) *CacheClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Minute}
	}
	return &CacheClient{client: client, sessionID: sessionID, http: httpClient, compression: codecGzip}
}

// Fetch implements runner.CacheClient.Fetch.
func (c *CacheClient) Fetch(ctx context.Context, workDir, runID, jobID string, entry *gocdnextv1.CacheEntry) (runner.CacheFetchStats, error) {
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
			return runner.CacheFetchStats{}, nil
		}
		return runner.CacheFetchStats{}, fmt.Errorf("request cache get: %w", err)
	}
	if !resp.GetFound() {
		return runner.CacheFetchStats{}, nil
	}
	restoreStart := time.Now()
	bytes, err := runner.DownloadAndUntar(ctx, c.http, resp.GetGetUrl(), workDir, resp.GetContentSha256())
	if err != nil {
		return runner.CacheFetchStats{}, fmt.Errorf("download+untar: %w", err)
	}
	return runner.CacheFetchStats{Found: true, Bytes: bytes, Restore: time.Since(restoreStart)}, nil
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
// produced this run, …), it uploads a valid ~23-byte empty
// tar.gz and marks the key ready — mirroring shared-mode
// TarGzPaths exactly (see storeEmptyCacheBlob). Refreshing the
// row to an empty blob is deliberate: it prevents the next run
// from restoring a stale tree left by an earlier run that DID
// produce the path. The empty tar.gz is a valid archive, so
// downstream Fetch untars it cleanly (no gzip-parse-fail).
//
// Returns the uploaded blob size so the runner can report it.
// Best-effort like Store: callers log on error and continue.
func (c *CacheClient) StoreFromPod(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, containerName, podWorkDir string,
	runID, jobID string,
	entry *gocdnextv1.CacheEntry,
) (runner.CacheStoreStats, error) {
	if len(entry.GetPaths()) == 0 {
		return runner.CacheStoreStats{}, errors.New("cache: entry has no paths")
	}
	if exec == nil {
		return runner.CacheStoreStats{}, errors.New("cache: nil executor")
	}

	// 1. Probe — list existing paths (defanged for leading-dash)
	//    INSIDE the pod. Single round-trip per cache entry.
	probeStart := time.Now()
	existing, err := c.probeCachePaths(ctx, exec, podName, containerName, podWorkDir, entry.GetPaths())
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("probe paths: %w", err)
	}
	probeDur := time.Since(probeStart)
	if len(existing) == 0 {
		// Mirror shared-mode semantics: TarGzPaths(workDir, nil)
		// produces a valid empty tar.gz (~23 bytes), which Store
		// then PUTs and marks ready. The cache row gets a fresh
		// empty blob, NOT preserved as whatever older entry was
		// there. Skipping the whole RPC sequence (as v0.5.6 did)
		// would silently keep a stale ready blob from an earlier
		// run when the job DID produce the path — a divergence
		// from shared mode that surprises operators.
		stats, err := c.storeEmptyCacheBlob(ctx, runID, jobID, entry.GetKey())
		stats.Probe = probeDur
		return stats, err
	}

	// 2. RPC + tar + PUT + ready — only when there's content.
	put, err := c.client.RequestCachePut(ctx, &gocdnextv1.RequestCachePutRequest{
		SessionId: c.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Key:       entry.GetKey(),
	})
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("request cache put: %w", err)
	}

	tmp, err := os.CreateTemp("", "gocdnext-cache-pod-*.tar.gz")
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("tempfile: %w", err)
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
	cmd := append([]string{"sh", "-c", cacheTarScript(c.compression), "_", podWorkDir}, existing...)
	// Compress = the in-pod tar+compress exec streamed into the agent temp.
	// Single-threaded gzip by default; zstd -T0 when GOCDNEXT_CACHE_COMPRESSION
	// selects it and the housekeeper image carries zstd (#274).
	compressStart := time.Now()
	if err := exec.Exec(ctx, podName, containerName, cmd, nil, mw, io.Discard); err != nil {
		_ = tmp.Close()
		return runner.CacheStoreStats{}, fmt.Errorf("exec tar %q: %w", entry.GetKey(), err)
	}

	info, statErr := tmp.Stat()
	if cerr := tmp.Close(); cerr != nil && statErr == nil {
		statErr = cerr
	}
	if statErr != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("stat tar tmp: %w", statErr)
	}
	compressDur := time.Since(compressStart)
	size := info.Size()

	body, err := os.Open(tmpName)
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = body.Close() }()

	uploadStart := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, put.GetPutUrl(), body)
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("build PUT: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", cacheContentType(c.compression))

	resp, err := c.http.Do(req)
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("http PUT: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return runner.CacheStoreStats{}, fmt.Errorf("PUT returned %s", resp.Status)
	}
	uploadDur := time.Since(uploadStart)

	markStart := time.Now()
	if _, err := c.client.MarkCacheReady(ctx, &gocdnextv1.MarkCacheReadyRequest{
		SessionId:     c.sessionID,
		CacheId:       put.GetCacheId(),
		SizeBytes:     size,
		ContentSha256: hex.EncodeToString(hasher.Sum(nil)),
	}); err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("mark cache ready: %w", err)
	}
	return runner.CacheStoreStats{
		Bytes:     size,
		Probe:     probeDur,
		Compress:  compressDur,
		Upload:    uploadDur,
		MarkReady: time.Since(markStart),
	}, nil
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
func (c *CacheClient) storeEmptyCacheBlob(ctx context.Context, runID, jobID, key string) (runner.CacheStoreStats, error) {
	var buf bytes.Buffer
	sha, size, err := runner.TarGzPaths("", nil, &buf)
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("empty tar: %w", err)
	}

	put, err := c.client.RequestCachePut(ctx, &gocdnextv1.RequestCachePutRequest{
		SessionId: c.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Key:       key,
	})
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("request cache put: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, put.GetPutUrl(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("build PUT: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := c.http.Do(req)
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("http PUT: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return runner.CacheStoreStats{}, fmt.Errorf("PUT returned %s", resp.Status)
	}

	if _, err := c.client.MarkCacheReady(ctx, &gocdnextv1.MarkCacheReadyRequest{
		SessionId:     c.sessionID,
		CacheId:       put.GetCacheId(),
		SizeBytes:     size,
		ContentSha256: sha,
	}); err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("mark cache ready: %w", err)
	}
	// Empty blob is ~23 bytes — negligible, so no per-phase split here.
	return runner.CacheStoreStats{Bytes: size}, nil
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

// Store implements runner.CacheClient.Store. Returns the uploaded
// blob size so the runner can report it in the store log line.
func (c *CacheClient) Store(ctx context.Context, workDir, runID, jobID string, entry *gocdnextv1.CacheEntry) (runner.CacheStoreStats, error) {
	if len(entry.GetPaths()) == 0 {
		// A key with no paths has no tarball to upload. The parser
		// already rejects this shape at pipeline apply time, but
		// guard here too — defence in depth against a future
		// assignment builder that forgets to copy paths through.
		return runner.CacheStoreStats{}, errors.New("cache: entry has no paths")
	}

	put, err := c.client.RequestCachePut(ctx, &gocdnextv1.RequestCachePutRequest{
		SessionId: c.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Key:       entry.GetKey(),
	})
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("request cache put: %w", err)
	}

	sha, size, compress, upload, err := runner.TarAndUpload(ctx, c.http, put.GetPutUrl(), workDir, entry.GetPaths())
	if err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("tar+upload: %w", err)
	}

	markStart := time.Now()
	if _, err := c.client.MarkCacheReady(ctx, &gocdnextv1.MarkCacheReadyRequest{
		SessionId:     c.sessionID,
		CacheId:       put.GetCacheId(),
		SizeBytes:     size,
		ContentSha256: sha,
	}); err != nil {
		return runner.CacheStoreStats{}, fmt.Errorf("mark cache ready: %w", err)
	}
	return runner.CacheStoreStats{
		Bytes:     size,
		Compress:  compress,
		Upload:    upload,
		MarkReady: time.Since(markStart),
	}, nil
}
