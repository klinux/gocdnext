package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/gocdnext/gocdnext/proto/cachekey"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

const (
	// MaxCacheKeyGlobFiles caps how many files a single `hash`
	// invocation can fold in. Above this, we fail loud rather
	// than quietly hashing thousands of files (a `**/*` typo
	// would otherwise spend minutes walking a node_modules tree
	// before returning a key the operator probably didn't mean).
	// Lives on the agent side because the resolver is the only
	// thing that resolves globs against a real workspace.
	MaxCacheKeyGlobFiles = 100

	// MaxCacheKeyHashBytesPerFile caps individual file size that
	// hash() will fold in. Lockfiles top out around 5–10 MiB even
	// in giant monorepos; 16 MiB leaves comfortable headroom
	// without letting a stray match (an artifact tarball glob'd
	// in by mistake) stall the dispatch.
	MaxCacheKeyHashBytesPerFile = 16 * 1024 * 1024

	// MaxCacheKeyHashBytesTotal caps total bytes hashed across
	// all files in a single hash() call. Up to 100 files (per
	// MaxCacheKeyGlobFiles) of MaxCacheKeyHashBytesPerFile would
	// allow 1.6 GiB per call in the worst case — way too much
	// time pre-tasks. 64 MiB total is more than any honest
	// lockfile+package.json glob needs.
	MaxCacheKeyHashBytesTotal = 64 * 1024 * 1024

	// hashReadChunk is the buffer size used by Hash when
	// streaming each file into the digest. 64 KiB is the standard
	// trade-off: large enough to amortize the per-Read overhead
	// on big files, small enough to give ctx-cancel quick
	// reaction.
	hashReadChunk = 64 * 1024
)

// expandCacheKeys walks assignment.caches in-place, re-parsing
// each key (the server already validated syntax at apply time;
// we re-parse because the server doesn't ship the parsed shape
// over the wire) and expanding any `{{ hash "..." }}` tokens
// using files materialised under workDir. Called AFTER checkout
// + before fetchCaches.
//
// Fail-loud: an unparseable template (server-agent version skew,
// somebody hand-edited cache.key in the DB) or a glob with zero
// matches aborts the job. Cache misses are normal; cache key
// computation errors aren't.
//
// Plain literals (no `{{`) round-trip unchanged via the HasTokens
// fast path — no filesystem touch, no allocation cost.
//
// ctx flows into the resolver so CancelJob propagates: a job
// canceled mid-hash (huge file, slow disk) aborts the read at
// the next chunk boundary rather than blocking until EOF.
func (r *Runner) expandCacheKeys(ctx context.Context, workDir string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) error {
	entries := a.GetCaches()
	if len(entries) == 0 {
		return nil
	}
	resolver := &workspaceHashResolver{ctx: ctx, workDir: workDir}
	for _, e := range entries {
		raw := e.GetKey()
		tpl, err := cachekey.Parse(raw)
		if err != nil {
			return fmt.Errorf("cache key %q: parse: %w", raw, err)
		}
		if !tpl.HasTokens() {
			// Literal, nothing to do. Skip the resolver entirely
			// so a workspace-read failure on an unrelated cache
			// entry can't corrupt this one.
			continue
		}
		expanded, err := tpl.Expand(resolver)
		if err != nil {
			return err
		}
		// Stamp the expanded form back onto the proto message.
		// fetchCaches + storeCaches read e.Key downstream, so
		// mutation-in-place is the right plumb.
		e.Key = expanded
		// Surface the expansion in the job log so the operator
		// can see what cache key the job is using on this run —
		// useful when debugging cache misses ("why did key change?").
		r.emitLog(a, seq, "stdout",
			fmt.Sprintf("cache key %q expanded to %q", raw, expanded))
	}
	return nil
}

// workspaceHashResolver implements cachekey.FunctionResolver.Hash
// by globbing the argument against the workspace and folding the
// matched files into a deterministic sha256.
//
// Determinism contract:
//   - filepath.Glob output is sorted lexicographically.
//   - Per file: hash content + path (separator-delimited).
//   - Final output is the first HashOutputLength hex chars.
//
// Same lockfile content + same path-set → same key, every time,
// across agents and across hosts. Different content OR different
// matched set → different key, which is the whole point.
//
// Hardening (added in v0.4.37 follow-up review):
//
//   - ctx propagates from the runner so CancelJob aborts a
//     slow/big hash between chunks instead of blocking until EOF.
//   - Per-file and total byte caps prevent a stray match (an
//     artifact tarball glob'd in by mistake) from stalling the
//     dispatch with gigabytes of pre-task hashing.
//   - Symlinks are REJECTED via lstat: a repo could otherwise
//     point its declared lockfile at /etc/passwd and the agent
//     would dutifully hash it. The 12-hex output is a weak
//     oracle, not a content leak, but it weakens the
//     "workspace-relative" guarantee the parser promises.
type workspaceHashResolver struct {
	ctx     context.Context
	workDir string

	// realWorkDir is workDir with every component's symlink
	// resolved. Computed lazily on the first Hash call so test
	// setups can construct the struct literal directly. Cached
	// because a single template can fire Hash many times.
	//
	// Containment check (per match) uses realWorkDir as the
	// anchor rather than workDir: an intermediate directory
	// symlink can otherwise escape (workspace/locks → /etc plus
	// arg `"locks/passwd"` would resolve to /etc/passwd, and a
	// raw Lstat on the user-typed path follows directory
	// symlinks transparently — passing leaf-is-regular while
	// reading outside the workspace).
	realWorkDir string
}

func (r *workspaceHashResolver) resolvedWorkDir() (string, error) {
	if r.realWorkDir != "" {
		return r.realWorkDir, nil
	}
	real, err := filepath.EvalSymlinks(r.workDir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace %q: %w", r.workDir, err)
	}
	r.realWorkDir = real
	return r.realWorkDir, nil
}

func (r *workspaceHashResolver) Hash(arg string) (string, error) {
	// Resolve glob relative to workspace. filepath.Glob is
	// well-defined for relative patterns when given an absolute
	// base; Join keeps the agent's workspace anchored.
	pattern := filepath.Join(r.workDir, arg)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("glob %q: %w", arg, err)
	}
	if len(matches) == 0 {
		// Operator declared invalidation by this file/glob; the
		// file isn't there. Silent fallback to "" or a constant
		// would let two distinct lockfiles share a key — exactly
		// the regression we're shipping the feature to prevent.
		return "", fmt.Errorf("glob %q matched 0 files in workspace", arg)
	}
	if len(matches) > MaxCacheKeyGlobFiles {
		return "", fmt.Errorf("glob %q matched %d files, exceeds max %d (narrow your pattern)",
			arg, len(matches), MaxCacheKeyGlobFiles)
	}
	// Deterministic order. filepath.Glob's order is filesystem-
	// dependent; sort to get a stable hash across boxes.
	sort.Strings(matches)

	realWorkDir, err := r.resolvedWorkDir()
	if err != nil {
		return "", fmt.Errorf("hash %q: %w", arg, err)
	}

	hasher := sha256.New()
	var totalBytes int64
	for _, m := range matches {
		// Cancel propagation: check at every file boundary BEFORE
		// the stat/open syscalls so a canceled job returns quickly
		// without doing any more I/O.
		if err := r.ctx.Err(); err != nil {
			return "", fmt.Errorf("hash %q: %w", arg, err)
		}
		// Relative path goes into the digest so renaming a file
		// changes the key (catches the case where a lockfile is
		// renamed without content changes).
		rel, err := filepath.Rel(r.workDir, m)
		if err != nil {
			rel = m
		}

		// Containment check: resolve EVERY symlink in the chain
		// (intermediate dirs + leaf) and verify the canonical
		// path is still inside the workspace. Catches the
		// directory-symlink escape that Lstat alone misses —
		// `workspace/locks → /etc` + `locks/passwd` resolves to
		// `/etc/passwd`, which Lstat happily reports as a regular
		// file because OS path resolution follows the directory
		// symlink before lstating the leaf.
		realPath, err := filepath.EvalSymlinks(m)
		if err != nil {
			return "", fmt.Errorf("hash %q: resolve %q: %w", arg, rel, err)
		}
		realRel, err := filepath.Rel(realWorkDir, realPath)
		if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("hash %q: %q resolves outside workspace (symlink escape detected — resolved %q is not under %q)",
				arg, rel, realPath, realWorkDir)
		}

		// Leaf-type check: even when the chain stays inside the
		// workspace, reject symlinks/fifos/device nodes at the
		// leaf. Keeps the cache key derivation a pure function of
		// regular-file contents and stops a leaf-symlink-within-
		// workspace from making the digest depend on the link
		// target's identity (a swap-the-target swap that breaks
		// determinism even when contained).
		info, err := os.Lstat(m)
		if err != nil {
			return "", fmt.Errorf("lstat %q: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("hash %q: %q is not a regular file (mode=%s; symlinks/special files are rejected to keep cache key derivation workspace-bound)",
				arg, rel, info.Mode())
		}

		fmt.Fprintf(hasher, "%s\n", rel)

		// Stream the file in chunks. Each chunk: ctx check, byte
		// cap check, hasher write. Chunks bound the cancel
		// reaction time to one buffer's worth of reading.
		read, err := streamHash(r.ctx, hasher, m, rel, MaxCacheKeyHashBytesPerFile, MaxCacheKeyHashBytesTotal-totalBytes)
		if err != nil {
			return "", err
		}
		totalBytes += read

		// Trailer between files so concatenation across files
		// can't accidentally form a different multi-file content.
		fmt.Fprintf(hasher, "\n")
	}
	full := hex.EncodeToString(hasher.Sum(nil))
	return full[:cachekey.HashOutputLength], nil
}

// streamHash reads `path` into `hasher` in fixed-size chunks,
// enforcing per-file + remaining-total byte caps and checking
// ctx between chunks. Returns bytes read so the caller can
// track the cumulative total across files. Errors carry the
// relative path (rel) for operator-friendly logs.
func streamHash(ctx context.Context, hasher io.Writer, path, rel string, perFileCap, remainingTotalCap int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %q: %w", rel, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, hashReadChunk)
	var read int64
	for {
		if err := ctx.Err(); err != nil {
			return read, fmt.Errorf("hash %q: %w", rel, err)
		}
		n, err := f.Read(buf)
		if n > 0 {
			read += int64(n)
			if read > perFileCap {
				return read, fmt.Errorf("hash %q: file size exceeds per-file cap %d bytes (narrow your glob or drop oversized matches)",
					rel, perFileCap)
			}
			if read > remainingTotalCap {
				return read, fmt.Errorf("hash %q: total bytes hashed exceeds %d (narrow your glob)",
					rel, MaxCacheKeyHashBytesTotal)
			}
			if _, werr := hasher.Write(buf[:n]); werr != nil {
				return read, fmt.Errorf("hash %q: %w", rel, werr)
			}
		}
		if err == io.EOF {
			return read, nil
		}
		if err != nil {
			return read, fmt.Errorf("read %q: %w", rel, err)
		}
	}
}
