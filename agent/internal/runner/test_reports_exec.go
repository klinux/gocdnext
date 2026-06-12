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
	"sync/atomic"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/podfs"
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

	files, err := podfs.ListFiles(ctx, exec, podName, container, workDir)
	if err != nil {
		r.emitLog(a, seq, "stderr", fmt.Sprintf("test_reports: list workspace files: %v", err))
		return
	}

	matches := podfs.MatchGlobs(workDir, files, globs)
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

// catPodFile execs `cat -- <path>` inside the housekeeper and
// returns the file bytes. Capped at maxSingleReportBytes so a
// runaway report can't pin the agent's memory.
//
// Returns (bytes, "") on success, (nil, warning) on cat failure or
// size overflow. The warning is forwarded to the job stderr stream
// so the operator can see which file blew up; the caller continues
// with the remaining files.
func catPodFile(ctx context.Context, exec engine.PodExecutor, pod, container, filePath string) ([]byte, string) {
	return catPodFileN(ctx, exec, pod, container, filePath, maxSingleReportBytes)
}

// catPodFileN is catPodFile with a caller-chosen byte cap — coverage
// profiles legitimately exceed the JUnit cap on large repos.
func catPodFileN(ctx context.Context, exec engine.PodExecutor, pod, container, filePath string, maxBytes int) ([]byte, string) {
	if filePath == "" || !path.IsAbs(filePath) {
		return nil, fmt.Sprintf("cat: filePath must be absolute, got %q", filePath)
	}
	stdout := &podfs.CappedBuffer{W: &bytes.Buffer{}, Max: maxBytes + 1}
	var stderr bytes.Buffer
	if err := exec.Exec(ctx, pod, container,
		[]string{"cat", "--", filePath},
		nil, stdout, &stderr,
	); err != nil {
		return nil, fmt.Sprintf("cat %s: %v (stderr=%q)", filePath, err, stderr.String())
	}
	if stdout.W.Len() > maxBytes {
		return nil, fmt.Sprintf("cat %s: exceeds %d bytes cap; truncating",
			filePath, maxBytes)
	}
	return stdout.W.Bytes(), ""
}
