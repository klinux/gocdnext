// Package runner — test_reports_exec.go reads JUnit XML reports
// from inside a running pod's housekeeper container via
// PodExecutor.Exec, for workspace-isolated mode. Shared-mode walks
// the agent host filesystem directly (test_reports.go::expandGlobs).
//
// Issue #15 — until v0.14.4 the isolated path emitted a warn log
// telling the operator to switch back to ReadWriteMany if they
// wanted per-case Tests-tab reporting. This file closes the gap.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
	"sync/atomic"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// maxSingleReportBytes caps how many bytes a single `cat` is
// allowed to return. JUnit XML can balloon when assertion diffs
// land in <failure> bodies (Kotlin/Spock readable-diff formatters
// inline 50-row tables). 8 MiB is generous; anything larger gets
// truncated and a warning surfaces so the operator can split the
// suite or trim the diff. The parser's per-field clamp
// (maxCaseFieldBytes) trims further before send.
const maxSingleReportBytes = 8 << 20

// maxReportFilesPerJob is the safety belt against a pathological
// build that drops thousands of fragment XMLs into a single test-
// results dir. We sort + truncate so the agent doesn't sit there
// catting tens of thousands of files. Real Gradle / Maven runs
// stay well under 1000 even on large monorepos.
const maxReportFilesPerJob = 1000

// scanTestReportsFromPod is the workspace-isolated equivalent of
// scanTestReports. Same contract: resolve every glob in the
// assignment's TestReports, decode each match as JUnit XML, ship
// one TestResultBatch back. Errors never fail the job — tests are
// observability, not part of the build contract.
//
// Glob resolution runs in two passes so we don't shell-expand the
// pattern inside the pod:
//
//  1. Exec `find <workDir> -type f` once and read the absolute
//     paths of every file in the pod's workspace.
//  2. Filter agent-side via doublestar.PathMatch against each
//     declared glob (joined under workDir). Same matcher
//     shared-mode uses, so the two paths agree on semantics
//     (`**` recursion included).
//
// Then for each surviving path, exec `cat -- <path>` to fetch the
// bytes and feed them to parseJUnitData.
func (r *Runner) scanTestReportsFromPod(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, container, workDir string,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) {
	globs := a.GetTestReports()
	if len(globs) == 0 {
		return
	}
	if exec == nil {
		r.emitLog(a, seq, "stderr", "test_reports: no pod executor wired; skipping")
		return
	}

	files, err := listPodFiles(ctx, exec, podName, container, workDir)
	if err != nil {
		r.emitLog(a, seq, "stderr", fmt.Sprintf("test_reports: list workspace files: %v", err))
		return
	}

	matches := matchPodFilesAgainstGlobs(workDir, files, globs)
	if len(matches) == 0 {
		// Silent — same posture as shared mode for "no files matched".
		return
	}
	if len(matches) > maxReportFilesPerJob {
		r.emitLog(a, seq, "stderr", fmt.Sprintf(
			"test_reports: glob matched %d files; truncating to %d",
			len(matches), maxReportFilesPerJob))
		matches = matches[:maxReportFilesPerJob]
	}

	var results []*gocdnextv1.TestResult
	for _, p := range matches {
		raw, warn := catPodFile(ctx, exec, podName, container, p)
		if warn != "" {
			r.emitLog(a, seq, "stderr", "test_reports: "+warn)
			continue
		}
		res, warns := parseJUnitData(p, raw)
		for _, w := range warns {
			r.emitLog(a, seq, "stderr", "test_reports: "+w)
		}
		results = append(results, res...)
	}
	if len(results) == 0 {
		return
	}

	batch := &gocdnextv1.TestResultBatch{
		RunId:   a.GetRunId(),
		JobId:   a.GetJobId(),
		Results: clampBatch(results),
	}
	r.cfg.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_TestResults{TestResults: batch},
	})
	r.emitLog(a, seq, "stdout",
		fmt.Sprintf("test_reports: shipped %d cases across %d file(s)",
			len(batch.Results), len(matches)))
}

// listPodFiles runs `find <workDir> -type f` inside the housekeeper
// and returns the absolute paths of every regular file in the
// workspace. Globbing is done agent-side (matchPodFilesAgainstGlobs)
// so a single `find` covers all declared patterns and we stay shell-
// agnostic — busybox (alpine), bash, dash all accept this invocation
// unchanged.
//
// Cap on stdout: a workspace with millions of files would balloon
// the buffer. 16 MiB is enough for ~150k file paths at ~100 chars
// each — well above any real CI workspace.
func listPodFiles(ctx context.Context, exec engine.PodExecutor, pod, container, workDir string) ([]string, error) {
	if workDir == "" || !path.IsAbs(workDir) {
		return nil, fmt.Errorf("workDir must be absolute, got %q", workDir)
	}
	var stdout, stderr bytes.Buffer
	stdoutCap := &cappedBuffer{w: &stdout, max: 16 << 20}
	if err := exec.Exec(ctx, pod, container,
		[]string{"find", workDir, "-type", "f"},
		nil, stdoutCap, &stderr,
	); err != nil {
		return nil, fmt.Errorf("exec find in %s/%s: %w (stderr=%q)",
			pod, container, err, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	out := lines[:0]
	for _, l := range lines {
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// matchPodFilesAgainstGlobs filters the file list down to those
// matching any of the declared globs (joined under workDir). Uses
// doublestar.PathMatch — same semantics as shared-mode
// expandGlobs — so a YAML pattern produces the same result set
// regardless of workspace mode.
//
// Deduplicates by absolute path. Empty patterns are skipped.
func matchPodFilesAgainstGlobs(workDir string, files, globs []string) []string {
	if len(files) == 0 || len(globs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(files))
	out := make([]string, 0, len(files))
	for _, g := range globs {
		if g == "" {
			continue
		}
		pat := path.Join(workDir, g)
		for _, f := range files {
			if seen[f] {
				continue
			}
			ok, err := doublestar.PathMatch(pat, f)
			if err != nil || !ok {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// catPodFile execs `cat -- <path>` inside the housekeeper and
// returns the file bytes. Capped at maxSingleReportBytes so a
// runaway report can't pin the agent's memory.
//
// Returns (bytes, "") on success, (nil, warning) on cat failure or
// size overflow. The warning is forwarded to the job stderr stream
// so the operator can see which file blew up; the caller continues
// with the remaining files.
func catPodFile(ctx context.Context, exec engine.PodExecutor, pod, container, filePath string) ([]byte, string) {
	if filePath == "" || !path.IsAbs(filePath) {
		return nil, fmt.Sprintf("cat: filePath must be absolute, got %q", filePath)
	}
	var stdout, stderr bytes.Buffer
	stdoutCap := &cappedBuffer{w: &stdout, max: maxSingleReportBytes + 1}
	if err := exec.Exec(ctx, pod, container,
		[]string{"cat", "--", filePath},
		nil, stdoutCap, &stderr,
	); err != nil {
		return nil, fmt.Sprintf("cat %s: %v (stderr=%q)", filePath, err, stderr.String())
	}
	if stdout.Len() > maxSingleReportBytes {
		return nil, fmt.Sprintf("cat %s: exceeds %d bytes cap; truncating",
			filePath, maxSingleReportBytes)
	}
	return stdout.Bytes(), ""
}

// cappedBuffer is a thin io.Writer that bounds a backing buffer at
// `max` bytes — extra bytes are silently dropped so the SPDY exec
// stream stays clean (closing the writer mid-stream would surface
// as a transport error, which is indistinguishable from a real
// network failure). Same trick as outputs_exec.go::capBuf, but
// uses an external bytes.Buffer so the caller can read it without
// the wrapper's API.
type cappedBuffer struct {
	w   *bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.max - c.w.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = c.w.Write(p[:remaining])
		return len(p), nil
	}
	return c.w.Write(p)
}
