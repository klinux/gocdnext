package runner

import (
	"bufio"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

func (r *Runner) streamLines(rd io.Reader, stream string, a *gocdnextv1.JobAssignment, seq *atomic.Int64, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(rd)
	// Raise buffer size: long `go test -v` lines or minified JS can blow past
	// the default 64 KiB.
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		r.emitLog(a, seq, stream, scanner.Text())
	}
	// Scanner errors (e.g., pipe close) are ignored: the Wait() below sees the
	// real process exit which is the authoritative outcome.
}

func (r *Runner) emitLog(a *gocdnextv1.JobAssignment, seq *atomic.Int64, stream, text string) {
	n := seq.Add(1)
	r.cfg.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Log{
			Log: &gocdnextv1.LogLine{
				RunId:  a.GetRunId(),
				JobId:  a.GetJobId(),
				Seq:    n,
				At:     timestamppb.New(time.Now().UTC()),
				Stream: stream,
				Text:   applyMasks(text, a.GetLogMasks()),
			},
		},
	})
}

// applyMasks replaces every occurrence of a secret value with "***". Masks
// of length < 4 are ignored so common short words don't accidentally get
// replaced (e.g. "a", "go", "and"). Multi-line values are matched per-line
// only — the scanner splits output on newlines before we see it, so a long
// PEM key might not be fully masked line-by-line. Known limit; the secret
// is still in the job's env either way.
func applyMasks(text string, masks []string) string {
	for _, m := range masks {
		if len(m) < 4 {
			continue
		}
		text = strings.ReplaceAll(text, m, "***")
	}
	return text
}

func (r *Runner) sendResult(a *gocdnextv1.JobAssignment, status gocdnextv1.RunStatus, exitCode int32, errMsg string) {
	r.sendResultWithArtifacts(a, status, exitCode, errMsg, nil)
}

func (r *Runner) sendResultWithArtifacts(a *gocdnextv1.JobAssignment, status gocdnextv1.RunStatus, exitCode int32, errMsg string, refs []*gocdnextv1.ArtifactRef) {
	r.sendResultWithArtifactsAndOutputs(a, status, exitCode, errMsg, refs, nil)
}

// sendResultWithArtifactsAndOutputs is the canonical result sender —
// the other two wrappers exist for call-site readability. Outputs
// (issue #10) is alias → value, already filtered + validated by
// parseOutputsFile against the job's declarations.
func (r *Runner) sendResultWithArtifactsAndOutputs(a *gocdnextv1.JobAssignment, status gocdnextv1.RunStatus, exitCode int32, errMsg string, refs []*gocdnextv1.ArtifactRef, outputs map[string]string) {
	r.cfg.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Result{
			Result: &gocdnextv1.JobResult{
				RunId:     a.GetRunId(),
				JobId:     a.GetJobId(),
				Status:    status,
				ExitCode:  exitCode,
				Error:     errMsg,
				Artifacts: refs,
				Outputs:   outputs,
			},
		},
	})
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// sanitize strips path separators so a hostile run_id/job_id can't escape
// the workspace root. run_ids and job_ids are UUIDs in production, but tests
// and future manual triggers may pass arbitrary strings.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '/', '\\', '.', 0:
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}
