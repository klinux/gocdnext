// Package runner — cachekey_pod_resolver.go provides a
// cachekey.FunctionResolver implementation that reads workspace
// files from inside a job pod's PVC via PodExecutor.Exec. Used by
// isolated-mode jobs whose cache keys carry `{{ hash "<glob>" }}`
// tokens that need workspace-side hashing.
//
// Mirrors the determinism contract of workspaceHashResolver
// (shared-mode counterpart):
//
//   - file enumeration is sorted lexicographically
//   - per file: hash(relPath + "\n" + content + "\n")
//   - same lockfile content + same path-set → same key
//
// Differences vs the host resolver, by necessity:
//
//   - Symlink resolution / containment: the host resolver does
//     EvalSymlinks + relativity check to block "workspace/lock →
//     /etc/passwd" escapes. We can't do that cheaply via exec —
//     and `find -type f` doesn't return symlinks anyway, so an
//     in-workspace symlink to /etc/passwd wouldn't appear in our
//     match set. The attack surface is also narrower because the
//     PVC is ephemeral per-job and the workspace contents come from
//     `git clone` (which doesn't create symlinks unless the repo
//     itself has them committed). Documented as a known v1
//     limitation.
//   - File I/O is via `cat <abs>` over the exec stream rather than
//     `os.Open`. Per-chunk ctx-check happens at the bytes.Buffer
//     write granularity inside CappedBuffer (the writer never
//     blocks on the cancelled ctx because the exec stream wraps it).
package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/podfs"
	"github.com/gocdnext/gocdnext/proto/cachekey"
)

// podHashResolver implements cachekey.FunctionResolver against a
// job pod's housekeeper sidecar. Constructed with the absolute
// workDir as seen from inside the container (typically the
// PodSpec's WorkingDir for the task), an enumerated set of files
// from one `find` call, and a PodExecutor for the per-file cats.
type podHashResolver struct {
	ctx       context.Context
	exec      engine.PodExecutor
	podName   string
	container string
	workDir   string

	// files is the cached output of `find <workDir> -type f`.
	// One enumeration per resolver invocation amortises across all
	// Hash() calls a template can fire (gradle.properties +
	// **/*.gradle + gradle/wrapper/* in one key, for example).
	// Lazily populated on the first Hash call.
	files []string
}

// newPodHashResolver wires the resolver to a specific pod's
// container. The workDir MUST be absolute and MUST be where the
// task is running — using the wrong root would either miss matches
// (workspace mount root when the task ran inside a target_dir) or
// over-match (mount root when the operator's glob is target_dir-
// relative). executeIsolated already computes the right value as
// `scriptWorkDir`.
func newPodHashResolver(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, container, workDir string,
) *podHashResolver {
	return &podHashResolver{
		ctx:       ctx,
		exec:      exec,
		podName:   podName,
		container: container,
		workDir:   workDir,
	}
}

// ensureListing populates the cached file enumeration on first call.
// Subsequent Hash() invocations reuse the same listing — saves a
// `find` per declared cache key when an operator uses multiple
// `hash(...)` tokens that all glob the same workspace.
func (p *podHashResolver) ensureListing() error {
	if p.files != nil {
		return nil
	}
	files, err := podfs.ListFiles(p.ctx, p.exec, p.podName, p.container, p.workDir)
	if err != nil {
		return fmt.Errorf("pod resolver: list files: %w", err)
	}
	p.files = files
	return nil
}

// Hash implements cachekey.FunctionResolver. Same shape as the
// host-side workspaceHashResolver.Hash but reads via cat-exec
// instead of os.Open, and uses doublestar.PathMatch (via podfs)
// instead of filepath.Glob.
//
// Errors:
//
//   - listing failure (find/exec error) → wrapped with context.
//   - zero matches → fail loud. The operator declared invalidation
//     by this file; a silent "" would let two different lockfiles
//     share a key, which is exactly the regression cache-key
//     templating exists to prevent. Same posture as the host
//     resolver.
//   - per-file cap / total cap exceeded → wrapped error naming the
//     offending file, mirroring the host resolver's error text so
//     operators see the same hint regardless of mode.
func (p *podHashResolver) Hash(arg string) (string, error) {
	if err := p.ctx.Err(); err != nil {
		return "", fmt.Errorf("hash %q: %w", arg, err)
	}
	if err := p.ensureListing(); err != nil {
		return "", fmt.Errorf("hash %q: %w", arg, err)
	}

	matches := podfs.MatchSingleGlob(p.workDir, p.files, arg)
	if len(matches) == 0 {
		return "", fmt.Errorf("glob %q matched 0 files in workspace", arg)
	}
	if len(matches) > MaxCacheKeyGlobFiles {
		return "", fmt.Errorf("glob %q matched %d files, exceeds max %d (narrow your pattern)",
			arg, len(matches), MaxCacheKeyGlobFiles)
	}
	// Deterministic order. podfs.MatchSingleGlob preserves the
	// enumeration order from `find`, which is filesystem-dependent
	// — sort here so the host and pod resolvers agree.
	sort.Strings(matches)

	hasher := sha256.New()
	var totalBytes int64
	for _, abs := range matches {
		if err := p.ctx.Err(); err != nil {
			return "", fmt.Errorf("hash %q: %w", arg, err)
		}
		// Workspace-relative path goes into the digest so renaming
		// a file changes the key — same contract as the host
		// resolver. path.Join strips trailing slashes etc., then we
		// trim the workDir prefix.
		rel := strings.TrimPrefix(abs, p.workDir)
		rel = strings.TrimPrefix(rel, "/")

		body, warn := p.readFile(abs)
		if warn != "" {
			return "", fmt.Errorf("hash %q: read %q: %s", arg, rel, warn)
		}
		if int64(len(body)) > MaxCacheKeyHashBytesPerFile {
			return "", fmt.Errorf("hash %q: file %q size exceeds per-file cap %d bytes (narrow your glob or drop oversized matches)",
				arg, rel, MaxCacheKeyHashBytesPerFile)
		}
		totalBytes += int64(len(body))
		if totalBytes > MaxCacheKeyHashBytesTotal {
			return "", fmt.Errorf("hash %q: total bytes hashed exceeds %d (narrow your glob)",
				arg, MaxCacheKeyHashBytesTotal)
		}

		fmt.Fprintf(hasher, "%s\n", rel)
		_, _ = hasher.Write(body)
		fmt.Fprintf(hasher, "\n")
	}
	full := hex.EncodeToString(hasher.Sum(nil))
	return full[:cachekey.HashOutputLength], nil
}

// readFile execs `cat -- <abs>` inside the container, bounded by
// MaxCacheKeyHashBytesPerFile+1 so we can detect overflow before
// returning to Hash. Bytes are returned as a slice (not a stream)
// because the digest needs the whole file and the host resolver's
// streaming optimisation doesn't transfer cleanly to exec — every
// extra chunk is a full SPDY frame, the cost is amortised at the
// `cat` level instead.
func (p *podHashResolver) readFile(absPath string) ([]byte, string) {
	if !path.IsAbs(absPath) {
		return nil, fmt.Sprintf("path must be absolute, got %q", absPath)
	}
	stdout := &podfs.CappedBuffer{W: &bytes.Buffer{}, Max: int(MaxCacheKeyHashBytesPerFile) + 1}
	var stderr bytes.Buffer
	if err := p.exec.Exec(p.ctx, p.podName, p.container,
		[]string{"cat", "--", absPath},
		nil, stdout, &stderr,
	); err != nil {
		return nil, fmt.Sprintf("cat %s: %v (stderr=%q)", absPath, err, stderr.String())
	}
	return stdout.W.Bytes(), ""
}

// Compile-time guard: the cachekey package's resolver interface
// only requires Hash. If a future token type adds another method
// the agent has to implement, this assertion breaks the build at
// the wire-up site instead of silently accepting a partial impl.
var _ cachekey.FunctionResolver = (*podHashResolver)(nil)
