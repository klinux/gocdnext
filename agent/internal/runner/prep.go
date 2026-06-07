// Package runner — prep.go houses the workspace materialisation
// logic that runs inside the "prep" init container in isolated
// workspace mode. The same code is invoked via the
// `gocdnext-agent prep` subcommand (cmd/gocdnext-agent/main.go),
// which deserialises a JobAssignment from a mounted Secret and
// calls Prep with the pod's mounted workspace as workspaceDir.
//
// In isolated mode the agent process never touches the job's
// workspace — it just builds the Pod spec. All filesystem work
// (clone, tar/untar, hash) happens INSIDE the job pod, against
// the pod's own ephemeral PVC. That's the entire point of
// isolated mode: jobs don't share workspace.
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// containsTemplate is a cheap probe for `{{ }}` tokens in a
// cache key. Used to distinguish "literal key, cache miss" from
// "templated key, not yet supported in isolated mode" without
// requiring the full cachekey.Parse round-trip.
func containsTemplate(key string) bool { return strings.Contains(key, "{{") }

// Prep runs the workspace materialisation phase of a job:
//   - Clone each declared material.
//   - Download each declared upstream artifact (pre-signed URLs
//     embedded in the JobAssignment by the agent at dispatch).
//   - Download each LITERAL-KEY cache entry whose ticket the
//     agent pre-resolved at dispatch (CacheEntry.fetch_url set).
//     Templated keys (`{{ hash "..." }}`) need workspace-side
//     hashing and are skipped with a warning.
//
// logWriter receives one plain-text line per progress event. K8s log
// collection prepends timestamps + container name when the operator
// `kubectl logs <pod> -c prep`, so the writer doesn't add its own.
//
// Returns the first error encountered; the init container's
// non-zero exit signals failure to the agent observer, which then
// reports a failed JobResult.
func Prep(ctx context.Context, a *gocdnextv1.JobAssignment, workspaceDir string, logWriter io.Writer) error {
	if a == nil {
		return fmt.Errorf("prep: nil assignment")
	}
	if workspaceDir == "" {
		return fmt.Errorf("prep: empty workspace dir")
	}
	abs, err := filepath.Abs(workspaceDir)
	if err != nil {
		return fmt.Errorf("prep: resolve workspace %q: %w", workspaceDir, err)
	}
	workspaceDir = abs

	prepLog(logWriter, "prep: starting (run=%s job=%s name=%s checkouts=%d artifact_downloads=%d caches=%d)",
		a.GetRunId(), a.GetJobId(), a.GetName(),
		len(a.GetCheckouts()), len(a.GetArtifactDownloads()), len(a.GetCaches()))

	// scriptWorkDir is the directory the user's task container will
	// cd into — the first checkout's target_dir, or the workspace
	// root if no checkouts. Artifact downloads land relative to it,
	// matching the shared-mode behaviour in Execute (runner.go).
	scriptWorkDir := workspaceDir

	for i, co := range a.GetCheckouts() {
		if err := Checkout(ctx, workspaceDir, co, logWriter); err != nil {
			return fmt.Errorf("checkout %s: %w", co.GetUrl(), err)
		}
		if i == 0 && co.GetTargetDir() != "" {
			scriptWorkDir = filepath.Join(workspaceDir, co.GetTargetDir())
		}
	}

	for _, dl := range a.GetArtifactDownloads() {
		if err := DownloadArtifact(ctx, scriptWorkDir, dl, logWriter); err != nil {
			return fmt.Errorf("artifact %s (from %s): %w", dl.GetPath(), dl.GetFromJob(), err)
		}
	}

	// Cache fetch: the agent pre-resolves literal cache keys at
	// dispatch (executeIsolated::ResolveGet) and populates
	// fetch_url/fetch_sha256/fetch_found on each CacheEntry. The
	// init container doesn't talk to the server here — it just
	// downloads + untars over scriptWorkDir. Templated keys
	// (`{{ hash "..." }}`) stay skipped because workspace files
	// only exist HERE, not at the agent at dispatch time.
	for _, entry := range a.GetCaches() {
		if entry.GetKey() == "" {
			continue
		}
		if !entry.GetFetchFound() {
			// Either a literal-key cache miss (normal cold start)
			// or a templated key we couldn't pre-resolve. Stay
			// silent on miss; warn loudly on the latter so the
			// operator sees why.
			if entry.GetFetchUrl() == "" && containsTemplate(entry.GetKey()) {
				prepLog(logWriter,
					"prep: warning — cache key %q has `{{ }}` tokens; "+
						"templated keys aren't yet supported in workspace "+
						"isolated mode (workspace-side hashing required, but "+
						"the init container has no gRPC session to call "+
						"RequestCacheGet). Cache skipped.",
					entry.GetKey())
			}
			continue
		}
		if err := DownloadAndUntar(ctx, nil, entry.GetFetchUrl(), scriptWorkDir, entry.GetFetchSha256()); err != nil {
			// Cache is acceleration, never correctness: log + carry on.
			prepLog(logWriter, "prep: cache %q: fetch failed (%v) — continuing without",
				entry.GetKey(), err)
			continue
		}
		prepLog(logWriter, "prep: cache %q: restored %d path(s)",
			entry.GetKey(), len(entry.GetPaths()))
	}

	// Outputs (issue #10 isolated parity): when the job declares
	// outputs:, create the directory + empty file that the agent
	// will GOCDNEXT_OUTPUT_FILE-point the task container at.
	//
	// The plugin writes via `> $GOCDNEXT_OUTPUT_FILE`, which
	// requires the parent dir to exist; without prep doing this,
	// the very first plugin run would fail with "No such file or
	// directory" before producing anything to parse.
	//
	// Permissions: world-writable (0o777 on dir, 0o666 on file).
	// The task container's UID is unknown — plugin images can ship
	// USER 65532 (distroless), 1000 (build images), or root. The
	// housekeeper that READS this file later runs as root in
	// alpine, so it can always read regardless. Worst case for
	// 0o666 is another container in the same pod tampering — but
	// the pod's PVC is per-job RWO ephemeral; no cross-tenant
	// surface to abuse.
	if len(a.GetOutputs()) > 0 {
		outputsRel := OutputsRelPath(a.GetJobId())
		outputsFull := filepath.Join(scriptWorkDir, outputsRel)
		outputsDir := filepath.Dir(outputsFull)            // .../<scriptWorkDir>/.gocdnext/outputs
		gocdnextDir := filepath.Dir(outputsDir)            // .../<scriptWorkDir>/.gocdnext

		if err := os.MkdirAll(outputsDir, 0o777); err != nil {
			return fmt.Errorf("mkdir outputs dir %s: %w", outputsDir, err)
		}

		// Chmod EACH component we just created. MkdirAll honours
		// the process umask (the prep init container inherits
		// pod-level defaults, often 0o022 or 0o077 in hardened
		// images), so even though we asked for 0o777, the parent
		// `.gocdnext` may have landed at 0o755 or 0o700 root-owned.
		// A non-root task USER would then fail to traverse it on
		// the first `> $GOCDNEXT_OUTPUT_FILE`. Both components
		// belong to gocdnext exclusively (artifacts go to
		// user-declared paths), so making them world-traversable
		// is safe and not over-broad.
		for _, dir := range []string{gocdnextDir, outputsDir} {
			if err := os.Chmod(dir, 0o777); err != nil {
				return fmt.Errorf("chmod %s: %w", dir, err)
			}
		}

		f, err := os.OpenFile(outputsFull, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
		if err != nil {
			return fmt.Errorf("touch outputs file %s: %w", outputsFull, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close outputs file %s: %w", outputsFull, err)
		}
		if err := os.Chmod(outputsFull, 0o666); err != nil {
			return fmt.Errorf("chmod outputs file %s: %w", outputsFull, err)
		}
		prepLog(logWriter, "prep: outputs file ready at %s (declared aliases: %d)",
			outputsRel, len(a.GetOutputs()))
	}

	prepLog(logWriter, "prep: workspace ready at %s", workspaceDir)
	return nil
}

// Checkout clones a single material into baseDir/<targetDir>.
// Streams git's stdout/stderr to logWriter line-by-line. Returns
// the first error from fork/exec or a non-zero git exit.
//
// Standalone (no Runner state) so the init container can invoke it
// directly. The shared-mode runner also delegates to this from its
// own checkout() wrapper for parity.
func Checkout(ctx context.Context, baseDir string, mc *gocdnextv1.MaterialCheckout, logWriter io.Writer) error {
	if mc == nil {
		return fmt.Errorf("nil checkout")
	}
	if mc.GetUrl() == "" {
		return fmt.Errorf("checkout missing url")
	}
	target := filepath.Join(baseDir, mc.GetTargetDir())

	args := []string{"clone", "--quiet"}
	if mc.GetBranch() != "" {
		args = append(args, "--branch", mc.GetBranch())
	}
	args = append(args, mc.GetUrl(), target)

	prepLog(logWriter, "$ git %v", redactCloneArgs(args, mc.GetUrl()))
	if code, err := runCommandTo(ctx, "", "git", args, nil, logWriter); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("git clone exited %d", code)
	}

	if rev := mc.GetRevision(); rev != "" {
		revArgs := []string{"-C", target, "checkout", "--quiet", rev}
		prepLog(logWriter, "$ git %v", revArgs)
		if code, err := runCommandTo(ctx, "", "git", revArgs, nil, logWriter); err != nil {
			return err
		} else if code != 0 {
			return fmt.Errorf("git checkout %s exited %d", rev, code)
		}
	}
	return nil
}

// DownloadArtifact fetches a single upstream artefact via its
// pre-signed GET URL, verifies sha256, and untars into
// baseDir/<dl.dest>. Standalone for init-container reuse.
func DownloadArtifact(ctx context.Context, baseDir string, dl *gocdnextv1.ArtifactDownload, logWriter io.Writer) error {
	if dl == nil {
		return fmt.Errorf("nil artifact download")
	}
	if dl.GetGetUrl() == "" {
		return fmt.Errorf("artifact download missing get_url")
	}
	prepLog(logWriter, "$ download artifact %s (from %s)", dl.GetPath(), dl.GetFromJob())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dl.GetGetUrl(), nil)
	if err != nil {
		return fmt.Errorf("build GET: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http GET: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET returned %s", resp.Status)
	}

	dest := dl.GetDest()
	if dest == "" {
		dest = "./"
	}
	destAbs := filepath.Join(baseDir, dest)
	if err := UntarGz(destAbs, resp.Body, dl.GetContentSha256()); err != nil {
		return err
	}
	prepLog(logWriter, "  unpacked into %s", dest)
	return nil
}

// prepLog writes one line to logWriter with a newline.
// Best-effort: a closed writer doesn't bubble up an error to the
// caller — log line loss is preferable to aborting the clone for it.
func prepLog(w io.Writer, format string, args ...interface{}) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// runCommandTo execs `name args...` and streams stdout/stderr to
// logWriter as lines. Returns exit code (0 on success) and an
// error ONLY for lifecycle problems (fork failed, unexpected wait
// error). A non-zero exit is NOT an error.
//
// Equivalent to Runner.runCommand but writes lines to an io.Writer
// instead of emitting LogLine protos.
func runCommandTo(ctx context.Context, dir, name string, args []string, env []string, logWriter io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = env
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go streamLinesTo(stdout, logWriter, &wg)
	go streamLinesTo(stderr, logWriter, &wg)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

func streamLinesTo(rd io.Reader, w io.Writer, wg *sync.WaitGroup) {
	defer wg.Done()
	if w == nil {
		_, _ = io.Copy(io.Discard, rd)
		return
	}
	scanner := bufio.NewScanner(rd)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		_, _ = fmt.Fprintln(w, scanner.Text())
	}
}

// redactCloneArgs returns a copy of `args` with the `url` value
// replaced by "<url>" — keeps potentially sensitive https URLs
// (with embedded tokens, though we currently don't inject those)
// out of the prep log. Operator-visible URLs come from the YAML;
// repos with credential-embedded URLs in materials shouldn't be
// echoed back into the pod log either way.
func redactCloneArgs(args []string, url string) []string {
	if url == "" {
		return args
	}
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if a == url {
			out[i] = "<url>"
		}
	}
	return out
}
