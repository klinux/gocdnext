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
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sync/atomic"
	"time"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// cacheFetchTerminateTimeout bounds how long we wait for the
// cache-fetch init container to exit after the agent has touched
// the marker file. The container's `until` loop polls every 0.2s,
// so 30s is two orders of magnitude past the expected ~200ms
// handshake — anything longer indicates a wedged kubelet or an
// OOMKilled init mid-loop. We'd rather surface that via a fast
// failure of the downstream WaitForTaskStarted than block the
// runner until the job-level deadline.
const cacheFetchTerminateTimeout = 30 * time.Second

// resolveAndFetchTemplatedCaches is the entry point Execute calls
// when the assignment carries at least one cache entry with a
// `{{` token. By the time we get here, prep has terminated
// successfully and the cache-fetch init container is running and
// blocked on the marker.
//
// Steps (each best-effort; partial failure logs and continues):
//  1. Wait for cache-fetch to be in Running state (~ms after
//     prep terminates; K8s schedules init containers sequentially).
//  2. Build a podHashResolver pointing at cache-fetch + workDir.
//  3. expandCacheKeysVia — mutates each templated entry's key in
//     place to its resolved form.
//  4. For each formerly-templated entry, call RequestCacheGet via
//     the agent's gRPC session.
//  5. On hit: HTTP GET the URL on the agent host, stream the body
//     into `tar -xzf - -C <workDir>` via exec stdin.
//  6. Touch the marker — cache-fetch exits, K8s starts the main
//     containers, task runs with caches in place.
//
// The marker touch happens in a deferred call so panics or early
// returns can't strand the pod.
// resolveAndFetchTemplatedCaches takes BOTH the mount root and the
// script's working dir because they serve different purposes:
//
//   - mountPath (= k.Config().WorkspaceMountPath, typically
//     `/workspace`): where the marker file lives and where caches
//     untar to. The init container's wait loop polls this same
//     path via engine.CacheFetchMarkerPath so a `target_dir`
//     checkout can't desync the touch from the wait. Caches are
//     workspace-global (a pnpm-store goes at the PVC root, not
//     inside a target_dir).
//   - workDir (= scriptWorkDir, typically `<mountPath>/<target_dir>`):
//     where the hash resolver runs `find` and reads files. The
//     operator's `hash "**/pnpm-lock.yaml"` glob is relative to
//     wherever the task actually runs, which IS target_dir-
//     dependent.
func (r *Runner) resolveAndFetchTemplatedCaches(
	ctx context.Context,
	k *engine.Kubernetes,
	exec engine.PodExecutor,
	podName, mountPath, workDir string,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) {
	defer func() {
		// Always touch the marker so cache-fetch exits and the task
		// can start. Even if everything above failed, we'd rather
		// run without the cache than wedge the job on the init
		// container.
		if err := touchCacheReadyMarker(ctx, exec, podName, mountPath); err != nil {
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
		//
		// Bounded: the `until` loop should observe the touched
		// marker on its next 0.2s poll, so 30s is enormous
		// headroom for that handshake. If we hit the cap, something
		// is wrong (kubelet stuck, container OOMKilled mid-loop) —
		// log loud and let downstream WaitForTaskStarted timeout
		// surface as the operator-visible error, instead of
		// blocking here until the job-level cancel/timeout fires.
		termCtx, cancelTerm := context.WithTimeout(ctx, cacheFetchTerminateTimeout)
		if _, err := k.WaitForInitTerminated(termCtx, podName,
			engine.CacheFetchInitContainerName); err != nil {
			r.cfg.Logger.Warn("runner: cache-fetch terminated wait failed",
				"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
			r.emitLog(a, seq, "stderr", fmt.Sprintf(
				"cache: cache-fetch init didn't exit within %s after marker touch (%v); "+
					"task may fail on startup if the init container hangs",
				cacheFetchTerminateTimeout, err))
		}
		cancelTerm()
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
		// Cache content untars at workDir — the SAME anchor the
		// post-job store uses (postjob.go: cfg.PodWorkDir). The
		// store tars from workDir, so the tarball's top-level
		// entries are paths declared by the operator (e.g.
		// `.gradle-home/caches`) RELATIVE TO workDir. Untaring
		// at workDir round-trips that relativity correctly.
		//
		// Pre-v0.14.11 this used mountPath under the (wrong)
		// theory that caches are workspace-global. The store
		// never matched that theory — it always tarred from
		// workDir — so a checkout with `target_dir:` like
		// `src/<hash>/` produced tarballs containing
		// `.gradle-home/...` that, on restore, landed at
		// `<mountPath>/.gradle-home/...`. The job's CWD was
		// `<mountPath>/src/<hash>/`, GRADLE_USER_HOME=.gradle-home
		// resolved to `<mountPath>/src/<hash>/.gradle-home/` —
		// empty, hence cache HIT + full re-download. Same shape
		// for any node_modules / pnpm-store / cargo cache once
		// target_dir is in play.
		// Restore with a single retry on a truncated transfer; a failed attempt
		// wipes the partial tree so the retry/rebuild starts clean (a truncated
		// untar leaves a half-unpacked tree — e.g. an incomplete Gradle dist —
		// that would otherwise poison the build). The helper logs the reason.
		if err := r.restoreCacheIntoPod(ctx, exec, podName, workDir, url, sha, entry, a, seq); err != nil {
			continue
		}
		r.emitLog(a, seq, "stdout", fmt.Sprintf(
			"cache %q: hit, content restored to workspace", entry.GetKey()))
	}
}

// cacheRestoreError wraps a failed in-pod cache restore, flagging whether the
// failure was a truncated TRANSFER (transientTransfer=true → a re-fetch may
// succeed) versus a corrupt STORED blob (false → the full bytes arrived with a
// matching sha but won't decompress; a re-fetch would return the same poison).
type cacheRestoreError struct {
	err               error
	transientTransfer bool
}

func (e *cacheRestoreError) Error() string { return e.err.Error() }
func (e *cacheRestoreError) Unwrap() error { return e.err }

// restoreCacheIntoPod fetches + untars one entry with a single retry on a
// truncated transfer. Each failed attempt wipes the partial tree first so the
// retry (or the subsequent cold rebuild) starts clean. A corrupt STORED blob is
// not retried — the job rebuilds and the post-job store overwrites the bad blob
// (so it self-heals without an invalidate RPC). The sha used to classify is
// already computed inline during the stream (TeeReader), so the happy path pays
// nothing; the drain+compare runs only on the rare failure. Returns nil on a
// successful restore; the caller treats a non-nil return as a miss.
func (r *Runner) restoreCacheIntoPod(
	ctx context.Context, exec engine.PodExecutor, podName, workDir, url, sha string,
	entry *gocdnextv1.CacheEntry, a *gocdnextv1.JobAssignment, seq *atomic.Int64,
) error {
	const maxAttempts = 2
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = streamCacheIntoPod(ctx, exec, podName, workDir, url, sha); err == nil {
			return nil
		}
		// Partial tree from the failed untar would poison a retry/rebuild.
		if werr := wipePodCachePaths(ctx, exec, podName, workDir, entry.GetPaths()); werr != nil {
			r.emitLog(a, seq, "stderr", fmt.Sprintf(
				"cache %q: WARNING could not remove partial restore (%v); "+
					"build may use corrupt cache content", entry.GetKey(), werr))
		}
		var cre *cacheRestoreError
		transient := errors.As(err, &cre) && cre.transientTransfer
		if transient && attempt < maxAttempts {
			r.emitLog(a, seq, "stderr", fmt.Sprintf(
				"cache %q: transfer truncated (%v) — retrying fetch (%d/%d)",
				entry.GetKey(), err, attempt+1, maxAttempts))
			continue
		}
		// Terminal: log WHICH kind so a recurrence is diagnosable.
		if transient {
			r.emitLog(a, seq, "stderr", fmt.Sprintf(
				"cache %q: transfer still truncated after %d attempts (%v) — task runs without cache",
				entry.GetKey(), maxAttempts, err))
		} else {
			r.emitLog(a, seq, "stderr", fmt.Sprintf(
				"cache %q: STORED blob corrupt (full bytes, sha matched, decompress failed: %v) — "+
					"task runs without cache; post-job store will overwrite it", entry.GetKey(), err))
		}
		return err
	}
	return err
}

// wipePodCachePaths removes a cache entry's declared paths under workDir inside
// the cache-fetch container, as argv (no shell → operator-declared paths can't
// inject). It is the cleanup after a FAILED restore: a truncated `tar -xf`
// leaves a partial tree (e.g. an incomplete Gradle dist), and leaving it makes
// the build use corrupt cache content instead of rebuilding clean. No-op when
// there are no paths to remove.
func wipePodCachePaths(ctx context.Context, exec engine.PodExecutor, podName, workDir string, paths []string) error {
	argv := []string{"rm", "-rf", "--"}
	for _, p := range paths {
		if p == "" {
			continue
		}
		argv = append(argv, path.Join(workDir, p))
	}
	if len(argv) == 3 { // nothing to remove
		return nil
	}
	var stderr bytes.Buffer
	if err := exec.Exec(ctx, podName, engine.CacheFetchInitContainerName,
		argv, nil, io.Discard, &stderr); err != nil {
		return fmt.Errorf("%w (stderr=%q)", err, stderr.String())
	}
	return nil
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
	// bufio lets us peek the compression magic without consuming it; the
	// decoder in the pod still sees the full stream, and the hasher (via
	// the tee) still sums every byte, so sha256 verification is unaffected.
	br := bufio.NewReaderSize(io.TeeReader(resp.Body, hasher), 512)
	magic, perr := br.Peek(4)
	if perr != nil && len(magic) < 2 {
		return fmt.Errorf("peek cache blob magic: %w", perr)
	}
	cmd, cerr := podUntarCmd(magic, workDir)
	if cerr != nil {
		return fmt.Errorf("cache blob %w", cerr)
	}

	// Exec into cache-fetch container, pipe the peeked body in as
	// stdin to the codec-appropriate untar command.
	var stderr bytes.Buffer
	if execErr := exec.Exec(ctx, podName, engine.CacheFetchInitContainerName,
		cmd, br, io.Discard, &stderr,
	); execErr != nil {
		// Finish reading the body THROUGH the hasher: the exec stops consuming
		// `br` the instant the in-pod decoder dies, so without this the hash
		// covers only the prefix it piped. Draining is cheap (no decompression,
		// just the remaining network read) and only on this rare failure path.
		// The completed hash discriminates the failure:
		//   - sha != stored → the TRANSFER truncated (signed-URL GET or exec
		//     stdin cut mid-stream) — transient, a re-fetch may succeed;
		//   - sha == stored → the full blob arrived intact but is itself a
		//     corrupt zstd (bad STORED blob) — a re-fetch is pointless.
		_, _ = io.Copy(io.Discard, br)
		transientTransfer := expectedSha != "" &&
			hex.EncodeToString(hasher.Sum(nil)) != expectedSha
		return &cacheRestoreError{
			transientTransfer: transientTransfer,
			err: fmt.Errorf("exec untar in cache-fetch: %w (stderr=%q)",
				execErr, stderr.String()),
		}
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
// init container's `until` loop polls. Two separate exec calls
// (mkdir + touch) instead of one `sh -c` so a marker path with
// shell metacharacters can't be exploited and a path with spaces
// works correctly. mountPath comes from the engine's
// WorkspaceMountPath config; deriving the marker from it via the
// shared CacheFetchMarkerPath helper guarantees the agent and the
// cache-fetch container's wait loop agree on the same absolute
// path even when the operator configured a non-default mount root
// or the job declared a checkout target_dir (the marker lives at
// the PVC root, not at scriptWorkDir).
func touchCacheReadyMarker(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, mountPath string,
) error {
	marker := engine.CacheFetchMarkerPath(mountPath)

	// Two argv-form execs — no shell string concatenation anywhere,
	// no risk of metacharacter interpretation in operator-supplied
	// paths.
	var stderr bytes.Buffer
	if err := exec.Exec(ctx, podName, engine.CacheFetchInitContainerName,
		[]string{"mkdir", "-p", path.Dir(marker)},
		nil, io.Discard, &stderr,
	); err != nil {
		return fmt.Errorf("mkdir marker dir: %w (stderr=%q)", err, stderr.String())
	}
	stderr.Reset()
	if err := exec.Exec(ctx, podName, engine.CacheFetchInitContainerName,
		[]string{"touch", marker},
		nil, io.Discard, &stderr,
	); err != nil {
		return fmt.Errorf("touch marker: %w (stderr=%q)", err, stderr.String())
	}
	return nil
}
