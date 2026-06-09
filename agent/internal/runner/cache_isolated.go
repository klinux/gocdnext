// Package runner — cache_isolated.go owns the post-prep, pre-task
// orchestration that resolves `{{ hash "..." }}` cache keys and
// populates the workspace from cache hits, all via PodExecutor.Exec
// into the `cache-fetch` init container.
//
// Why a second init container (not the housekeeper): the marker-
// based handshake needs the container to stay alive while the agent
// runs, then EXIT so K8s starts the main task. Housekeeper lives in
// parallel with task and never exits during the job — wrong shape
// for "block task startup". The cache-fetch container's command is
// `until [ -f /workspace/<marker> ]; do sleep 0.2; done` so a single
// `touch` from the agent exits it.
//
// Failure posture: caches are an acceleration, not a correctness
// signal. Any error in this path (list/cat exec failure, hash
// computation error, RequestCacheGet RPC failure, stream-untar
// failure) ALWAYS ends with the marker being touched so the task
// can start. The job runs without the cache; the post-task store
// path rebuilds it on success — same as a cold-cache first run.
package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// resolveAndFetchTemplatedCaches is the entry point Execute calls
// when the assignment carries at least one cache entry with a
// `{{` token. By the time we get here, prep has terminated
// successfully and the cache-fetch init container is running and
// blocked on the marker.
//
// Steps (each best-effort; partial failure logs and continues):
//   1. Wait for cache-fetch to be in Running state (~ms after
//      prep terminates; K8s schedules init containers sequentially).
//   2. Build a podHashResolver pointing at cache-fetch + workDir.
//   3. expandCacheKeysVia — mutates each templated entry's key in
//      place to its resolved form.
//   4. For each formerly-templated entry, call RequestCacheGet via
//      the agent's gRPC session.
//   5. On hit: HTTP GET the URL on the agent host, stream the body
//      into `tar -xzf - -C <workDir>` via exec stdin.
//   6. Touch the marker — cache-fetch exits, K8s starts the main
//      containers, task runs with caches in place.
//
// The marker touch happens in a deferred call so panics or early
// returns can't strand the pod.
func (r *Runner) resolveAndFetchTemplatedCaches(
	ctx context.Context,
	k *engine.Kubernetes,
	exec engine.PodExecutor,
	podName, workDir string,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) {
	defer func() {
		// Always touch the marker so cache-fetch exits and the task
		// can start. Even if everything above failed, we'd rather
		// run without the cache than wedge the job on the init
		// container.
		if err := touchCacheReadyMarker(ctx, exec, podName, workDir); err != nil {
			r.cfg.Logger.Warn("runner: cache-fetch marker touch failed",
				"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
			r.emitLog(a, seq, "stderr", fmt.Sprintf(
				"cache: failed to signal cache-fetch container; "+
					"task may hang waiting for marker: %v", err))
		}
		// Wait for the init container to actually terminate so the
		// downstream WaitForTaskStarted poll doesn't race against
		// the still-running cache-fetch (which would make WAIT spin
		// until StartupTimeout — task hasn't started because init
		// hasn't ended yet).
		if _, err := k.WaitForInitTerminated(ctx, podName,
			engine.CacheFetchInitContainerName); err != nil {
			r.cfg.Logger.Warn("runner: cache-fetch terminated wait failed",
				"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
		}
	}()

	if err := k.WaitForInitStarted(ctx, podName, engine.CacheFetchInitContainerName); err != nil {
		r.cfg.Logger.Warn("runner: cache-fetch init container did not start",
			"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
		r.emitLog(a, seq, "stderr", fmt.Sprintf(
			"cache: cache-fetch init container did not start (%v); "+
				"templated caches skipped", err))
		return
	}

	resolver := newPodHashResolver(ctx, exec, podName, engine.CacheFetchInitContainerName, workDir)
	if err := r.expandCacheKeysVia(resolver, a, seq); err != nil {
		// expandCacheKeysVia's failures (parse / cap / glob-zero)
		// are loud by design in shared mode because the operator
		// asked for invalidation by these files. In isolated mode
		// we keep the same posture: surface the error, skip the
		// fetch, run task without the cache. Downgraded from a job-
		// killing error because cache is acceleration.
		r.emitLog(a, seq, "stderr", fmt.Sprintf("cache resolution: %v", err))
		return
	}

	if r.cfg.IsolatedCache == nil {
		// No cache client wired — same shape as the pre-resolve
		// path above for literal keys. Just log and exit.
		r.emitLog(a, seq, "stdout", "cache: no IsolatedCache wired; resolved keys not fetched")
		return
	}

	for _, entry := range a.GetCaches() {
		if entry.GetKey() == "" {
			continue
		}
		if entry.GetFetchUrl() != "" {
			// Already pre-resolved as a literal earlier in
			// executeIsolated and prep already fetched it. Skip.
			continue
		}
		url, sha, found, rerr := r.cfg.IsolatedCache.ResolveGet(ctx,
			a.GetRunId(), a.GetJobId(), entry.GetKey())
		if rerr != nil {
			r.emitLog(a, seq, "stderr", fmt.Sprintf(
				"cache %q: lookup failed (%v) — task runs without cache",
				entry.GetKey(), rerr))
			continue
		}
		if !found {
			r.emitLog(a, seq, "stdout", fmt.Sprintf(
				"cache %q: miss (post-resolution) — task will rebuild",
				entry.GetKey()))
			continue
		}
		entry.FetchFound = true
		entry.FetchUrl = url
		entry.FetchSha256 = sha
		if err := streamCacheIntoPod(ctx, exec, podName, workDir, url, sha); err != nil {
			r.emitLog(a, seq, "stderr", fmt.Sprintf(
				"cache %q: fetch failed (%v) — task runs without cache",
				entry.GetKey(), err))
			continue
		}
		r.emitLog(a, seq, "stdout", fmt.Sprintf(
			"cache %q: hit, content restored to workspace", entry.GetKey()))
	}
}

// streamCacheIntoPod HTTP-GETs the cache tarball on the agent host
// and pipes the body into `tar -xzf - -C <workDir>` via exec stdin
// inside the cache-fetch init container. Verifies sha256 on the
// fly so a corrupt blob doesn't poison the workspace silently.
//
// Why agent-side HTTP: cache-fetch is an alpine container with
// no network requirements declared in the chart. The agent already
// has connectivity to the cache backend's signed URL host (S3 /
// GCS / our filesystem backend). Piping via stdin keeps the bytes
// off the agent's disk too — they stream straight through.
func streamCacheIntoPod(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, workDir, url, expectedSha string,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build GET: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET cache blob: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("cache blob GET returned %s", resp.Status)
	}

	hasher := sha256.New()
	tee := io.TeeReader(resp.Body, hasher)

	// Exec into cache-fetch container, pipe the tee'd body in as
	// stdin to tar.
	var stderr bytes.Buffer
	if err := exec.Exec(ctx, podName, engine.CacheFetchInitContainerName,
		[]string{"tar", "-xzf", "-", "-C", workDir},
		tee, io.Discard, &stderr,
	); err != nil {
		return fmt.Errorf("exec tar in cache-fetch: %w (stderr=%q)", err, stderr.String())
	}

	if expectedSha != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != expectedSha {
			return fmt.Errorf("cache blob sha256 mismatch: got %s, want %s", got, expectedSha)
		}
	}
	return nil
}

// touchCacheReadyMarker `touch`es the marker file the cache-fetch
// init container's `until` loop polls. Uses `mkdir -p` for the
// directory because prep may have created the workspace layout
// without the .gocdnext subdir (e.g., a no-checkout job).
func touchCacheReadyMarker(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, workDir string,
) error {
	marker := workDir + "/" + engine.CacheFetchReadyMarker
	// `dirname` + `mkdir -p` inside one `sh -c` so the marker dir
	// is guaranteed to exist on systems where prep didn't create it.
	cmd := []string{
		"sh", "-c",
		"mkdir -p $(dirname " + marker + ") && touch " + marker,
	}
	var stderr bytes.Buffer
	if err := exec.Exec(ctx, podName, engine.CacheFetchInitContainerName,
		cmd, nil, io.Discard, &stderr,
	); err != nil {
		return fmt.Errorf("touch marker: %w (stderr=%q)", err, stderr.String())
	}
	return nil
}
