package rpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/podfs"
	"github.com/gocdnext/gocdnext/agent/internal/runner"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// ArtifactUploader implements runner.ArtifactUploader by calling
// RequestArtifactUpload on the gRPC client and then streaming the
// tar+gz of each path to the returned PUT URL over HTTP.
//
// Two-stage flow per path:
//  1. tar+gz to a temp file in order to know the Content-Length up
//     front (S3/GCS refuse chunked uploads); compute sha256 during
//     the write for free.
//  2. PUT the temp file with Content-Length set; delete on completion.
type ArtifactUploader struct {
	client    gocdnextv1.AgentServiceClient
	sessionID string
	http      *http.Client
	// compression is the STORE-side codec for the isolated in-pod tar
	// ("gzip" default, "zstd" opt-in via GOCDNEXT_ARTIFACT_COMPRESSION).
	// Restore is codec-agnostic — runner.UntarGz sniffs the blob — so this
	// only decides how NEW artifacts are written. gzip on already-compressed
	// artifacts (a .zip, a tar.gz, an image layer) is pure CPU waste; zstd's
	// fast path stores incompressible blocks near memcpy speed (#274).
	compression string
	// directUpload, when true (GOCDNEXT_ARTIFACT_DIRECT_UPLOAD), makes the
	// isolated-mode upload PUT straight from the pod to the signed URL via
	// curl in the housekeeper, instead of streaming the tar through the
	// agent's exec channel. The exec channel (SPDY via the apiserver) caps
	// around ~20 MB/s regardless of disk/network; a direct pod→object-store
	// PUT runs at the pod's network speed (multi-GB artifacts drop from
	// minutes to seconds). Opt-in because it needs (a) a housekeeper image
	// with curl and (b) an object store that accepts chunked PUT on its
	// signed URL (GCS does — it doesn't sign Content-Length; S3 does not).
	// The agent probes for curl and falls back to the exec-stream path when
	// it's absent, so a mixed-image fleet during rollout never breaks.
	directUpload bool
}

// UseDirectUpload enables the pod→store direct-PUT path from an operator
// string ("true"/"1"/"yes"/"on"). Empty/false keeps the exec-stream path.
// See the directUpload field for the contract (GCS + curl-capable housekeeper).
func (u *ArtifactUploader) UseDirectUpload(v string) {
	s := strings.ToLower(strings.TrimSpace(v))
	u.directUpload = s == "true" || s == "1" || s == "yes" || s == "on"
}

// UseCompression sets the store codec from an operator string, falling back
// to gzip for empty/unknown values so a typo never selects a codec the
// housekeeper image can't run. Reuses the cache codec constants + Content-Type
// helper (pure codec→value, not cache-specific).
func (u *ArtifactUploader) UseCompression(codec string) {
	if strings.EqualFold(strings.TrimSpace(codec), codecZstd) {
		u.compression = codecZstd
		return
	}
	u.compression = codecGzip
}

// artifactTarCmd builds the in-pod tar command for one declared path's files.
// gzip is byte-identical to the pre-zstd direct exec (`tar -czf`, no shell);
// zstd pipes tar into `zstd -T0` under a pipefail shell so a tar failure on a
// truncated stream isn't masked by zstd exiting 0. podWorkDir + files reach the
// shell as positional args ($1 = dir, $@ = files after shift), never
// interpolated into the script — an operator-controlled path can't inject.
func (u *ArtifactUploader) artifactTarCmd(podWorkDir string, files []string) []string {
	if u.compression == codecZstd {
		const script = `set -o pipefail; dir="$1"; shift; ` +
			`tar -cf - -C "$dir" -- "$@" | zstd -T0 -3 -c`
		return append([]string{"sh", "-c", script, "_", podWorkDir}, files...)
	}
	return append([]string{"tar", "-czf", "-", "-C", podWorkDir, "--"}, files...)
}

// directUploadMarker prefixes the single summary line the in-pod direct-upload
// command prints on stdout: "GOCDNEXTUP <sha256> <size>".
const directUploadMarker = "GOCDNEXTUP"

// directUploadCmd builds the in-pod command that tars+compresses one artifact
// path's files and PUTs the stream STRAIGHT to the signed URL via curl — the
// bytes go pod→object-store, never through the agent's exec channel. sha256 is
// computed inline through a fifo (the server cross-checks it against its own
// InspectObject), size comes from curl's %{size_upload}. The command emits a
// single `GOCDNEXTUP <sha> <size>` line on stdout and nothing else there.
//
// podWorkDir, putURL and files reach the shell as positional args ($1,$2,$@
// after `shift 2`) — never interpolated — so an operator-controlled path or a
// signed URL carrying shell metacharacters can't inject. The compressor +
// Content-Type are code constants (not input). curl -T - streams the body with
// chunked transfer-encoding (no Content-Length), which the signed URL must
// accept (GCS does).
func (u *ArtifactUploader) directUploadCmd(podWorkDir, putURL string, files []string) []string {
	compressor := "gzip -c"
	ctype := "application/gzip"
	if u.compression == codecZstd {
		compressor = "zstd -T0 -3 -c"
		ctype = "application/zstd"
	}
	script := `set -o pipefail; d="$1"; url="$2"; shift 2; ` +
		`f="$(mktemp -u)"; mkfifo "$f"; ( sha256sum <"$f" | cut -d' ' -f1 >"$f.sha" ) & ` +
		`sz="$(tar -cf - -C "$d" -- "$@" | ` + compressor + ` | tee "$f" | ` +
		`curl -fsS -X PUT -T - -H "Content-Type: ` + ctype + `" -w "%{size_upload}" -o /dev/null "$url")"; ` +
		`wait; printf "` + directUploadMarker + ` %s %s\n" "$(cat "$f.sha")" "$sz"; rm -f "$f" "$f.sha"`
	return append([]string{"sh", "-c", script, "_", podWorkDir, putURL}, files...)
}

// parseDirectUploadSummary extracts (sha256, size) from the direct-upload
// command's stdout — the last `GOCDNEXTUP <sha> <size>` line. Anything else on
// stdout is ignored so a stray warning doesn't break parsing; a missing or
// malformed marker is an error (we must not report a bogus ref the server
// would reject on the sha/size cross-check).
func parseDirectUploadSummary(stdout string) (string, int64, error) {
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != directUploadMarker {
			continue
		}
		sha := fields[1]
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("parse size %q: %w", fields[2], err)
		}
		if len(sha) != 64 {
			return "", 0, fmt.Errorf("sha256 %q is not 64 hex chars", sha)
		}
		return sha, size, nil
	}
	return "", 0, fmt.Errorf("no %s summary line in output", directUploadMarker)
}

// podHasCurl probes the housekeeper for curl once per upload batch. When
// directUpload is on but the image predates the curl addition (rolling fleet),
// this returns false and the caller keeps the exec-stream path — no regression.
func (u *ArtifactUploader) podHasCurl(ctx context.Context, exec engine.PodExecutor, podName, containerName string) bool {
	err := exec.Exec(ctx, podName, containerName,
		[]string{"sh", "-c", "command -v curl >/dev/null 2>&1"}, nil, io.Discard, io.Discard)
	return err == nil
}

// uploadOneFromPodDirect execs the direct-PUT command and turns its summary
// line into an ArtifactRef. The blob never crosses the exec channel.
func (u *ArtifactUploader) uploadOneFromPodDirect(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, containerName, podWorkDir string,
	tkt *gocdnextv1.ArtifactUploadTicket,
	files []string,
) (*gocdnextv1.ArtifactRef, error) {
	if len(files) == 0 {
		files = []string{tkt.GetPath()}
	}
	var out, errb strings.Builder
	cmd := u.directUploadCmd(podWorkDir, tkt.GetPutUrl(), files)
	if err := exec.Exec(ctx, podName, containerName, cmd, nil, &out, &errb); err != nil {
		return nil, fmt.Errorf("exec direct upload %q: %w (stderr=%q)", tkt.GetPath(), err, errb.String())
	}
	sha, size, err := parseDirectUploadSummary(out.String())
	if err != nil {
		return nil, fmt.Errorf("direct upload %q: %w (stderr=%q)", tkt.GetPath(), err, errb.String())
	}
	return &gocdnextv1.ArtifactRef{
		Path:          tkt.GetPath(),
		StorageKey:    tkt.GetStorageKey(),
		Size:          size,
		ContentSha256: sha,
	}, nil
}

// NewArtifactUploader wires the concrete uploader. http is optional;
// nil uses a default client with a 30-min timeout (big binaries take
// real time).
func NewArtifactUploader(client gocdnextv1.AgentServiceClient, sessionID string, httpClient *http.Client) *ArtifactUploader {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Minute}
	}
	return &ArtifactUploader{client: client, sessionID: sessionID, http: httpClient, compression: codecGzip}
}

// canonicalPath strips trailing slashes so `dist` and `dist/`
// collapse to the same key for the agent-side dedupe. Mirrors the
// server's store.NormalizeArtifactPath — kept inline (4 lines)
// instead of importing the server module from the agent.
func canonicalPath(p string) string {
	for len(p) > 1 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}

// Upload implements runner.ArtifactUploader.
//
// Paths are deduped by canonical form BEFORE the RPC so the server's
// own dedupe (added with the partial-unique-index in migration 00035)
// receives a stable count and returns exactly len(unique) tickets.
// Without this, a YAML that listed `dist` and `dist/` would land
// here as 2 paths, the server would return 1 ticket, and the
// `len(tickets) != len(paths)` check below would refuse the whole
// upload — silently failing a job for a duplicate-path operator
// typo. First-occurrence shape wins (matches server) so the
// returned ArtifactRef.Path round-trips the YAML's exact text.
func (u *ArtifactUploader) Upload(ctx context.Context, workDir, runID, jobID string, paths []string) ([]*gocdnextv1.ArtifactRef, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(paths))
	unique := make([]string, 0, len(paths))
	for _, p := range paths {
		canon := canonicalPath(p)
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		unique = append(unique, p)
	}

	resp, err := u.client.RequestArtifactUpload(ctx, &gocdnextv1.RequestArtifactUploadRequest{
		SessionId: u.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Paths:     unique,
	})
	if err != nil {
		return nil, fmt.Errorf("request upload: %w", err)
	}
	if got := len(resp.GetTickets()); got != len(unique) {
		return nil, fmt.Errorf("server returned %d tickets for %d paths", got, len(unique))
	}

	refs := make([]*gocdnextv1.ArtifactRef, 0, len(unique))
	for _, tkt := range resp.GetTickets() {
		ref, err := u.uploadOne(ctx, workDir, tkt)
		if err != nil {
			// Partial success returns what succeeded; caller decides
			// whether a missing ref is fatal. For now the runner logs
			// the error and continues (artifacts are best-effort).
			return refs, fmt.Errorf("upload %q: %w", tkt.GetPath(), err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// UploadFromPod is the isolated-mode counterpart of Upload. Same
// gRPC RequestArtifactUpload dance for tickets; the tar source is
// inside the job pod's housekeeper sidecar instead of the agent's
// local workDir. Each tarball is streamed through exec into a
// local temp file (S3/GCS refuse chunked PUTs, so Content-Length
// must be known up front — same constraint Upload hits).
//
// Dedupe + ticket alignment match Upload (kept inline; same logic).
//
// Returns refs for every successful upload + the first transport/
// exec error if any path failed.
func (u *ArtifactUploader) UploadFromPod(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, containerName, podWorkDir string,
	runID, jobID string,
	paths []string,
) ([]*gocdnextv1.ArtifactRef, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if exec == nil {
		return nil, fmt.Errorf("upload from pod: nil executor")
	}

	seen := make(map[string]struct{}, len(paths))
	unique := make([]string, 0, len(paths))
	for _, p := range paths {
		canon := canonicalPath(p)
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		unique = append(unique, p)
	}

	// Glob expansion (issue #16). Before v0.14.6 the agent invoked
	// `tar -czf - -- <declared-path>` and let tar receive a literal
	// `*`/`?`/etc — the housekeeper exec doesn't run a shell, so
	// `dist/*.jar` was treated as a filename, tar errored, and the
	// artifact silently vanished. Resolve agent-side via
	// podfs.MatchSingleGlob so the tar invocation receives concrete
	// workspace-relative paths.
	//
	// Declared paths with no glob characters short-circuit: no
	// `find` round-trip when nobody asked for one. Declared paths
	// with glob characters but zero matches are skipped entirely —
	// we don't even ask the server for a ticket. The caller decides
	// required-vs-optional semantics; this is upstream of that.
	resolved, listingErr := resolveArtifactGlobs(ctx, exec, podName, containerName, podWorkDir, unique)
	if listingErr != nil {
		// `find` itself failed (housekeeper dead, network glitch).
		// Bail loud — without the listing we'd have to either upload
		// nothing or fall back to tar-with-glob which is the bug we
		// just fixed.
		return nil, fmt.Errorf("resolve artifact globs in pod: %w", listingErr)
	}

	// Build the paths we'll actually request tickets for: declared
	// paths that survived resolution. Preserves operator-typed text
	// so the ArtifactRef.path round-trips back to the YAML.
	//
	// Zero-match declared paths surface as an error rather than a
	// silent skip — required-vs-optional semantics live one layer
	// up in PostJob (which swallows the error on the optional path
	// and bubbles it on the required one). Returning success here
	// would let `dist/*.tar.gz` as a REQUIRED artifact pass with no
	// upload, which used to fail loud under the pre-glob shape.
	requested := make([]string, 0, len(unique))
	missing := make([]string, 0, len(unique))
	for _, p := range unique {
		if files, ok := resolved[p]; ok && len(files) > 0 {
			requested = append(requested, p)
		} else {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return nil, &podfs.PathsMissingError{Paths: missing}
	}

	resp, err := u.client.RequestArtifactUpload(ctx, &gocdnextv1.RequestArtifactUploadRequest{
		SessionId: u.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Paths:     requested,
	})
	if err != nil {
		return nil, fmt.Errorf("request upload: %w", err)
	}
	if got := len(resp.GetTickets()); got != len(requested) {
		return nil, fmt.Errorf("server returned %d tickets for %d paths", got, len(requested))
	}

	// Choose the upload path once for the whole batch: direct pod→store PUT
	// when the operator opted in AND the housekeeper actually has curl (a
	// rolling fleet may still run the old image — fall back cleanly rather
	// than fail). Probe once, not per-ticket.
	useDirect := u.directUpload && u.podHasCurl(ctx, exec, podName, containerName)

	refs := make([]*gocdnextv1.ArtifactRef, 0, len(requested))
	for _, tkt := range resp.GetTickets() {
		files := resolved[tkt.GetPath()]
		var (
			ref *gocdnextv1.ArtifactRef
			err error
		)
		if useDirect {
			ref, err = u.uploadOneFromPodDirect(ctx, exec, podName, containerName, podWorkDir, tkt, files)
		} else {
			ref, err = u.uploadOneFromPod(ctx, exec, podName, containerName, podWorkDir, tkt, files)
		}
		if err != nil {
			return refs, fmt.Errorf("upload %q: %w", tkt.GetPath(), err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// ArtifactPathsMissingError is the legacy name from v0.14.6.
// Aliased to podfs.PathsMissingError so existing call sites still
// compile while the type lives in a package both rpc and runner
// can import. Removing the alias is a v0.15 follow-up; the
// wording on UploadFromPod's call sites already routes via
// errors.As against podfs.PathsMissingError.
type ArtifactPathsMissingError = podfs.PathsMissingError

// resolveArtifactGlobs expands every declared path against the pod
// workspace. Returns a map of declared → resolved relative paths
// (workspace-relative, so they slot straight into tar's `-C
// podWorkDir -- <rel>` form). Literal paths (no glob chars) round-
// trip as `{declared: [declared]}`.
//
// One `find -type f` per call — shared across all declared paths.
// Saves N-1 exec round-trips when N paths are declared.
func resolveArtifactGlobs(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, containerName, podWorkDir string,
	paths []string,
) (map[string][]string, error) {
	if len(paths) == 0 {
		return map[string][]string{}, nil
	}

	// Detect whether ANY declared path has glob characters; skip the
	// `find` round-trip entirely when every path is a literal.
	needsListing := false
	for _, p := range paths {
		if podfs.HasGlobChars(p) {
			needsListing = true
			break
		}
	}

	var allFiles []string
	if needsListing {
		var err error
		allFiles, err = podfs.ListFiles(ctx, exec, podName, containerName, podWorkDir)
		if err != nil {
			return nil, err
		}
	}

	resolved := make(map[string][]string, len(paths))
	for _, declared := range paths {
		if !podfs.HasGlobChars(declared) {
			// Literal — keep as-is. tar will error if the file doesn't
			// exist; that error is the operator's signal.
			resolved[declared] = []string{declared}
			continue
		}
		matches := podfs.MatchSingleGlob(podWorkDir, allFiles, declared)
		if len(matches) == 0 {
			// Zero matches — don't include in the result map. The
			// caller filters declared paths against keys-present.
			continue
		}
		// Convert absolute paths back to workspace-relative so tar
		// can be invoked as `tar -czf - -C podWorkDir -- <rel> ...`.
		rels := make([]string, 0, len(matches))
		for _, abs := range matches {
			rel := strings.TrimPrefix(abs, podWorkDir)
			rel = strings.TrimPrefix(rel, "/")
			rels = append(rels, rel)
		}
		resolved[declared] = rels
	}
	return resolved, nil
}

// _ uses `path` so the unused-import gate is silenced if a future
// edit removes the call below — keeps the import block stable
// against drift.
var _ = path.Clean

// uploadOneFromPod tars one declared path inside the pod via exec,
// streams through a temp file (to derive Content-Length), then PUTs.
// Sha256 is computed during the exec → tmp copy so the
// ArtifactRef.ContentSha256 matches what the server would receive
// even though no bytes pass through u.http until the PUT.
//
// `files` carries the resolved workspace-relative paths for the
// ticket's declared path — for literal paths it's `[declared]`, for
// glob paths it's the resolveArtifactGlobs match set. Passing them
// as separate args means tar bundles them all into ONE archive per
// declared path, preserving the operator's 1-declared-path-=-
// 1-artifact mental model.
func (u *ArtifactUploader) uploadOneFromPod(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, containerName, podWorkDir string,
	tkt *gocdnextv1.ArtifactUploadTicket,
	files []string,
) (*gocdnextv1.ArtifactRef, error) {
	if len(files) == 0 {
		// Defensive: UploadFromPod filters zero-match paths before
		// requesting tickets, so we shouldn't reach here. If we did,
		// fall back to the declared path so the failure mode is the
		// same as pre-v0.14.6 (tar errors → caller decides).
		files = []string{tkt.GetPath()}
	}
	tmp, err := os.CreateTemp("", "gocdnext-artifact-pod-*.tar.gz")
	if err != nil {
		return nil, fmt.Errorf("tempfile: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)

	// `--` separator before the operands so a path beginning with
	// `-` (e.g. an operator path like `-dist`) isn't reinterpreted
	// as a tar option. Combined with the agent-side dedupe + the
	// server validating paths, but cheap belt-and-suspenders.
	cmd := u.artifactTarCmd(podWorkDir, files)
	if err := exec.Exec(ctx, podName, containerName, cmd, nil, mw, io.Discard); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("exec tar %q: %w", tkt.GetPath(), err)
	}

	info, statErr := tmp.Stat()
	if cerr := tmp.Close(); cerr != nil && statErr == nil {
		statErr = cerr
	}
	if statErr != nil {
		return nil, fmt.Errorf("stat tar tmp: %w", statErr)
	}
	size := info.Size()

	body, err := os.Open(tmpName)
	if err != nil {
		return nil, fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = body.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, tkt.GetPutUrl(), body)
	if err != nil {
		return nil, fmt.Errorf("build PUT: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", cacheContentType(u.compression))

	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http PUT: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("PUT returned %s", resp.Status)
	}
	return &gocdnextv1.ArtifactRef{
		Path:          tkt.GetPath(),
		StorageKey:    tkt.GetStorageKey(),
		Size:          size,
		ContentSha256: hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

func (u *ArtifactUploader) uploadOne(ctx context.Context, workDir string, tkt *gocdnextv1.ArtifactUploadTicket) (*gocdnextv1.ArtifactRef, error) {
	tmp, err := os.CreateTemp("", "gocdnext-artifact-*.tar.gz")
	if err != nil {
		return nil, fmt.Errorf("tempfile: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	sha, size, err := runner.TarGzPath(workDir, tkt.GetPath(), tmp)
	if cerr := tmp.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("tar: %w", err)
	}

	body, err := os.Open(tmpName)
	if err != nil {
		return nil, fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = body.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, tkt.GetPutUrl(), body)
	if err != nil {
		return nil, fmt.Errorf("build PUT: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http PUT: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("PUT returned %s", resp.Status)
	}
	return &gocdnextv1.ArtifactRef{
		Path:          tkt.GetPath(),
		StorageKey:    tkt.GetStorageKey(),
		Size:          size,
		ContentSha256: sha,
	}, nil
}
