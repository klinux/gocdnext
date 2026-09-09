package runner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// CacheStoreStats is the per-phase breakdown of a cache store, so an
// operator can see WHERE the time went (issue #274): compression vs upload
// is what decides whether pigz/more-CPU helps or the bottleneck is transport.
// Probe is isolated-mode only (0 for shared). Compress covers tar+gzip (in
// isolated mode the in-pod `tar -czf` exec + stream to the agent temp).
type CacheStoreStats struct {
	Bytes     int64
	Probe     time.Duration
	Compress  time.Duration
	Upload    time.Duration
	MarkReady time.Duration
}

// CacheFetchStats is the restore-side counterpart. Download and untar are
// pipelined (untar reads the HTTP body as it streams), so they are reported
// as one Restore duration plus the byte count for a MB/s figure.
type CacheFetchStats struct {
	Found   bool
	Bytes   int64
	Restore time.Duration
}

// mbps renders a throughput figure for a phase; "-" when the duration is
// too small to be meaningful, so a sub-millisecond phase doesn't print a
// nonsense number.
func mbps(bytes int64, d time.Duration) string {
	if d < time.Millisecond || bytes <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f MB/s", float64(bytes)/(1<<20)/d.Seconds())
}

// IsolatedCacheClient is the isolated-mode counterpart of
// CacheClient. In isolated mode the init container has no gRPC
// session, so the agent pre-resolves cache GET URLs at dispatch
// time (ResolveGet) and embeds them in CacheEntry.fetch_url so
// the init container can HTTP-GET directly. Cache store goes via
// PodExecutor (StoreFromPod) — tar inside the housekeeper, PUT to
// signed URL, then MarkCacheReady from the agent's session.
//
// Templated keys (`{{ hash "..." }}`) need workspace files to
// expand, which only exist inside the pod, so they stay skipped
// in isolated mode until job-scoped session tokens land.
type IsolatedCacheClient interface {
	ResolveGet(ctx context.Context, runID, jobID, key string) (url, sha string, found bool, err error)
	StoreFromPod(ctx context.Context, exec engine.PodExecutor, podName, container, podWorkDir, runID, jobID string, entry *gocdnextv1.CacheEntry) (CacheStoreStats, error)
}

// CacheClient is how the runner talks to the server-side cache
// machinery. Kept behind an interface so runner tests can inject
// a fake without standing up a gRPC server. The concrete impl
// lives in agent/internal/rpc and drives RequestCacheGet /
// RequestCachePut / MarkCacheReady + the signed HTTP transfers.
type CacheClient interface {
	// Fetch downloads the cache blob identified by `entry.Key`
	// into `workDir` and untars it over the declared paths.
	// Returns found=false on cold-start miss (no ready row yet);
	// runner treats that as "run without pre-populated dir" and
	// moves on, NOT as an error. An actual transport or untar
	// failure returns an error the runner logs but does not
	// escalate into job failure.
	Fetch(ctx context.Context, workDir, runID, jobID string, entry *gocdnextv1.CacheEntry) (CacheFetchStats, error)

	// Store tars + uploads `entry.Paths` under `entry.Key`, then
	// calls MarkCacheReady so the next job on the same key can
	// Fetch it. Returns the uploaded blob size in bytes so the
	// caller can report it (a big GOCACHE upload is slow; the size
	// explains the duration). Best-effort: caller logs on error but
	// does not fail the job — the build succeeded, the cache miss
	// just costs the next run a cold rebuild.
	Store(ctx context.Context, workDir, runID, jobID string, entry *gocdnextv1.CacheEntry) (CacheStoreStats, error)
}

// fetchCaches runs before any task starts. For each declared
// CacheEntry we ask the server for a signed GET and untar the
// payload into scriptWorkDir. Misses are logged and ignored —
// they're the norm on first run for a key and the whole point
// of the design (agent proceeds without a pre-populated dir).
// A non-miss error (bad URL, sha mismatch, tar traversal) gets
// logged but ALSO doesn't fail the job: cache is acceleration
// and the worst-case cost of a transport hiccup is "rebuild
// everything", not "corrupt build". A fail-open default keeps
// the CI predictable even when the cache backend is flaky.
func (r *Runner) fetchCaches(
	ctx context.Context,
	workDir string,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) {
	entries := a.GetCaches()
	if len(entries) == 0 || r.cfg.Cache == nil {
		return
	}
	for _, e := range entries {
		if e.GetKey() == "" {
			// Client-side guard: an empty key would round-trip to the
			// server and come back as InvalidArgument. Skip locally
			// with a loud log so the operator sees the bad config.
			r.emitLog(a, seq, "stderr", "cache: skipping entry with empty key")
			continue
		}
		st, err := r.cfg.Cache.Fetch(ctx, workDir, a.GetRunId(), a.GetJobId(), e)
		switch {
		case err != nil:
			r.emitLog(a, seq, "stderr", fmt.Sprintf("cache %q: fetch failed (%v) — continuing without", e.GetKey(), err))
		case !st.Found:
			r.emitLog(a, seq, "stdout", fmt.Sprintf("cache %q: miss, no pre-populated dir", e.GetKey()))
		default:
			// #274 instrumentation: surface restore bytes + throughput so the
			// cache tax (fetch+untar, on the critical path to job start) is visible.
			r.emitLog(a, seq, "stdout", fmt.Sprintf("cache %q: restored %d path(s) (%s in %s, %s)",
				e.GetKey(), len(e.GetPaths()), humanizeBytes(st.Bytes), st.Restore.Round(time.Millisecond), mbps(st.Bytes, st.Restore)))
		}
	}
}

// storeCaches runs after a job's tasks succeed. Each declared
// CacheEntry gets tar'd + uploaded; failures log but don't fail
// the job. By the time we get here the build's outputs are
// already being uploaded as artifacts — caches are purely for
// the NEXT run's speed, not for this run's correctness.
func (r *Runner) storeCaches(
	ctx context.Context,
	workDir string,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) {
	entries := a.GetCaches()
	if len(entries) == 0 || r.cfg.Cache == nil {
		return
	}
	for _, e := range entries {
		if e.GetKey() == "" {
			continue
		}
		// Announce the start: tar + upload of a large cache runs for
		// minutes with no other output, which reads as a hung job.
		r.emitLog(a, seq, "stdout", fmt.Sprintf("cache %q: storing %d path(s)…", e.GetKey(), len(e.GetPaths())))
		start := time.Now()
		st, err := r.cfg.Cache.Store(ctx, workDir, a.GetRunId(), a.GetJobId(), e)
		if err != nil {
			r.emitLog(a, seq, "stderr", fmt.Sprintf("cache %q: store failed (%v) — next run will rebuild", e.GetKey(), err))
			continue
		}
		// #274 instrumentation: split compress vs upload — this is the number
		// that decides whether pigz/more-CPU helps (compress-bound) or the
		// bottleneck is transport (upload-bound).
		r.emitLog(a, seq, "stdout", fmt.Sprintf("cache %q: stored (%s in %s | compress %s %s, upload %s %s)",
			e.GetKey(), humanizeBytes(st.Bytes), phaseDur(start),
			st.Compress.Round(time.Millisecond), mbps(st.Bytes, st.Compress),
			st.Upload.Round(time.Millisecond), mbps(st.Bytes, st.Upload)))
	}
}

// humanizeBytes renders a byte count for log lines: "0 B", "512 B",
// "2.0 KB", "234.5 MB". Decimal (1000-based) so the numbers line up
// with what operators see in cloud-console object sizes, not 1024.
func humanizeBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// DownloadAndUntar is a helper the concrete CacheClient uses
// to fetch a signed URL and untar the payload on top of workDir.
// Exposed so the rpc package doesn't need to reimplement the
// verified-sha untar logic; the runner already got it right for
// artifact downloads.
// DownloadAndUntar returns the number of (compressed) bytes read so the
// restore log can report a MB/s figure (#274). Download and untar are
// pipelined here, so the caller times the whole call as one Restore phase.
func DownloadAndUntar(ctx context.Context, httpClient *http.Client, url, workDir, wantSHA string) (int64, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build GET: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("http GET: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("GET returned %s", resp.Status)
	}
	counter := &countingWriter{}
	if err := UntarGz(workDir, io.TeeReader(resp.Body, counter), wantSHA); err != nil {
		return counter.n, err
	}
	return counter.n, nil
}

// TarAndUpload is the mirror helper for cache uploads. Tars the
// declared paths into a single gzip stream (one blob per key),
// PUTs it at `url`, returns the total byte count + sha so the
// caller can pass them to MarkCacheReady. Writes to a temp file
// to know Content-Length up front — S3 signed PUTs refuse
// chunked transfers, same constraint the artifact uploader hit.
// TarAndUpload returns the tar+gzip (compress) and PUT (upload) durations
// separately (#274) so the shared-mode store can report the same split the
// isolated path does.
func TarAndUpload(ctx context.Context, httpClient *http.Client, url, workDir string, paths []string) (sha string, size int64, compress, upload time.Duration, err error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Minute}
	}
	tmp, err := os.CreateTemp("", "gocdnext-cache-*.tar.gz")
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("tempfile: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	compressStart := time.Now()
	sha, size, err = TarGzPaths(workDir, paths, tmp)
	if cerr := tmp.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("tar: %w", err)
	}
	compress = time.Since(compressStart)

	body, err := os.Open(tmpName)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = body.Close() }()

	uploadStart := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("build PUT: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("http PUT: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", 0, 0, 0, fmt.Errorf("PUT returned %s", resp.Status)
	}
	return sha, size, compress, time.Since(uploadStart), nil
}
