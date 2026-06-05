package rpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
// gRPC RequestCachePut → PUT → MarkCacheReady dance.
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

	// tar -czf - -C <podWorkDir> -- <path1> <path2> ... The
	// `--` separator avoids paths starting with `-` being read as
	// flags (same defence as the artifact path tar).
	cmd := append([]string{"tar", "-czf", "-", "-C", podWorkDir, "--"}, entry.GetPaths()...)
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
