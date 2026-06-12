// Package runner — outputs_exec.go reads $GOCDNEXT_OUTPUT_FILE
// from inside a running pod's housekeeper container via
// PodExecutor.Exec, for workspace-isolated mode (issue #10
// follow-up parity). Shared-mode reads the file directly off the
// agent host filesystem in runner.go::executeShared.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// outputsExecBufCap caps the buffer the exec'd `cat` is allowed to
// fill. One byte over the parser's outputsCapBytes so an oversize
// file is detectable (we read cap+1; if Len() > cap, it's over).
//
// A misbehaving plugin can't bloat agent memory by writing 10MB to
// the outputs file — capBuf silently discards beyond cap+1, and
// the parser refuses to ingest the result.
const outputsExecBufCap = outputsCapBytes + 1

// capBuf is an io.Writer that accepts up to `max` bytes and
// silently discards the rest (without erroring — returning an
// error mid-stream would surface to the SPDY exec as a broken
// pipe and propagate as an Exec error, which the caller can't
// distinguish from a genuine network failure).
//
// The "did we overflow?" signal is the post-Exec Len() check:
// Len() > outputsCapBytes → file exceeded the contract.
type capBuf struct {
	buf bytes.Buffer
	max int
}

func (c *capBuf) Write(p []byte) (int, error) {
	remaining := c.max - c.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = c.buf.Write(p[:remaining])
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capBuf) Len() int            { return c.buf.Len() }
func (c *capBuf) AsReader() io.Reader { return bytes.NewReader(c.buf.Bytes()) }

// ReadOutputsFromPod execs `cat -- <containerPath>` inside the
// housekeeper sidecar, captures stdout into a capped buffer, and
// parses the contents via parseOutputsReader against the declared
// alias map.
//
// containerPath MUST be the pod-side absolute path the agent set
// in GOCDNEXT_OUTPUT_FILE (mountPath + OutputsRelPath). It is
// never user-controlled — derived from the job ID via
// OutputsRelPath — so `--` is used out of habit-of-hygiene only,
// not because shell-meta injection is plausible here.
//
// Errors:
//   - cat exec failure (housekeeper dead, network glitch, file
//     unreadable) → wrapped Exec error; caller treats as job
//     failure same as artifact upload failures
//   - file > outputsCapBytes → "outputs file is too large" error
//   - parse failure → wrapped parser error (line N: ...)
//   - declared alias missing → parser returns the existing
//     "plugin did not write declared output(s)" message
//
// Returns the validated alias→value map identical in shape to
// what parseOutputsFile returns in shared mode, so the caller can
// ship it through sendResultWithArtifactsAndOutputs unchanged.
func ReadOutputsFromPod(
	ctx context.Context,
	exec engine.PodExecutor,
	pod, container, containerPath string,
	declared map[string]string,
) (map[string]string, error) {
	if exec == nil {
		return nil, fmt.Errorf("read outputs from pod: nil PodExecutor")
	}
	if len(declared) == 0 {
		return nil, nil
	}
	if containerPath == "" || !path.IsAbs(containerPath) {
		return nil, fmt.Errorf("read outputs from pod: containerPath must be absolute, got %q", containerPath)
	}

	stdout := &capBuf{max: outputsExecBufCap}
	var stderr bytes.Buffer

	// `cat --` keeps the hygiene-by-default habit shared with the
	// artifact path's `tar --` style. The path is fixed by the
	// agent (never user input), but `--` makes the call robust if
	// someone refactors the path derivation to include leading
	// dashes by accident.
	if err := exec.Exec(ctx, pod, container,
		[]string{"cat", "--", containerPath},
		nil, stdout, &stderr,
	); err != nil {
		return nil, fmt.Errorf("exec cat outputs in pod %s/%s: %w (stderr=%q)",
			pod, container, err, stderr.String())
	}

	if stdout.Len() > outputsCapBytes {
		return nil, fmt.Errorf(
			"outputs file exceeds %d bytes cap (read at least %d) — split large blobs into artifacts instead",
			outputsCapBytes, stdout.Len())
	}

	parsed, err := parseOutputsReader(stdout.AsReader(), declared)
	if err != nil {
		return nil, fmt.Errorf("parse outputs from pod %s/%s: %w", pod, container, err)
	}
	return parsed, nil
}
