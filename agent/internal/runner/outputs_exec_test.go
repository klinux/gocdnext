package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// fakePodExecutor stubs engine.PodExecutor. The test sets `body`
// (the bytes the cat would have written) and `cmdErr` (the error
// the exec layer would have returned). On Exec, both the body and
// the error are surfaced as if a real cat ran inside the
// housekeeper.
type fakePodExecutor struct {
	body      []byte
	cmdErr    error
	gotCmd    []string
	gotPod    string
	gotCont   string
	stderrOut []byte
}

func (f *fakePodExecutor) Exec(_ context.Context, pod, container string, cmd []string,
	_ io.Reader, stdout, stderr io.Writer) error {
	f.gotPod = pod
	f.gotCont = container
	f.gotCmd = append([]string(nil), cmd...)
	if len(f.body) > 0 {
		_, _ = stdout.Write(f.body)
	}
	if len(f.stderrOut) > 0 {
		_, _ = stderr.Write(f.stderrOut)
	}
	return f.cmdErr
}

func TestReadOutputsFromPod_HappyPath(t *testing.T) {
	exec := &fakePodExecutor{body: []byte("NEXT='1.2.3'\nKIND='patch'\n")}
	declared := map[string]string{"next": "NEXT"} // operator only declared `next`
	got, err := ReadOutputsFromPod(context.Background(), exec,
		"pod-X", "housekeeper", "/workspace/.gocdnext/outputs/abc.env", declared)
	if err != nil {
		t.Fatalf("ReadOutputsFromPod: %v", err)
	}
	if got["next"] != "1.2.3" {
		t.Errorf("next = %q, want 1.2.3", got["next"])
	}
	if _, leak := got["kind"]; leak {
		t.Errorf("undeclared output `kind` leaked through filter: %v", got)
	}

	// Exec contract: `cat --` with hygiene flag, absolute path,
	// inside the housekeeper container.
	if exec.gotPod != "pod-X" || exec.gotCont != "housekeeper" {
		t.Errorf("target: pod=%q cont=%q", exec.gotPod, exec.gotCont)
	}
	if len(exec.gotCmd) != 3 || exec.gotCmd[0] != "cat" || exec.gotCmd[1] != "--" {
		t.Errorf("cmd: want [cat -- /…], got %v", exec.gotCmd)
	}
}

func TestReadOutputsFromPod_ExecError(t *testing.T) {
	// Housekeeper container died, network glitch, file unreadable.
	// Surfaced as a wrapped error including stderr (operator
	// debugging hint), distinct from "no match" / "missing alias".
	exec := &fakePodExecutor{
		cmdErr:    errors.New("container 'housekeeper' not found"),
		stderrOut: []byte("cat: /workspace/.gocdnext/outputs/abc.env: No such file"),
	}
	_, err := ReadOutputsFromPod(context.Background(), exec,
		"pod-X", "housekeeper", "/workspace/.gocdnext/outputs/abc.env", map[string]string{"next": "NEXT"})
	if err == nil {
		t.Fatal("want error from exec failure, got nil")
	}
	if !strings.Contains(err.Error(), "exec cat outputs") {
		t.Errorf("error should wrap exec context, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "No such file") {
		t.Errorf("error should surface stderr to help operator debug, got %q", err.Error())
	}
}

func TestReadOutputsFromPod_ExceedsCap(t *testing.T) {
	// Plugin wrote > 64KB. capBuf silently discards beyond
	// cap+1; ReadOutputsFromPod checks Len() against the parser
	// cap and rejects loud. Parser never sees the oversize body.
	big := bytes.Repeat([]byte("X"), outputsCapBytes+10*1024)
	body := append([]byte("HUGE='"), big...)
	body = append(body, []byte("'\n")...)
	exec := &fakePodExecutor{body: body}
	_, err := ReadOutputsFromPod(context.Background(), exec,
		"pod-X", "housekeeper", "/workspace/.gocdnext/outputs/abc.env",
		map[string]string{"huge": "HUGE"})
	if err == nil {
		t.Fatal("want cap error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention cap, got %q", err.Error())
	}
}

func TestReadOutputsFromPod_MissingDeclaredAlias(t *testing.T) {
	// Plugin wrote A but operator declared `b: B`. Parser
	// surfaces the existing "plugin did not write declared
	// output(s)" message — wrapped here with pod context so the
	// operator knows where to look.
	exec := &fakePodExecutor{body: []byte("A='1'\n")}
	_, err := ReadOutputsFromPod(context.Background(), exec,
		"pod-X", "housekeeper", "/workspace/.gocdnext/outputs/abc.env",
		map[string]string{"b": "B"})
	if err == nil {
		t.Fatal("want declared-missing error, got nil")
	}
	if !strings.Contains(err.Error(), "did not write declared output") {
		t.Errorf("error should mention declared output, got %q", err.Error())
	}
}

func TestReadOutputsFromPod_MalformedLine(t *testing.T) {
	exec := &fakePodExecutor{body: []byte("NEXT 1.2.3\n")} // missing =
	_, err := ReadOutputsFromPod(context.Background(), exec,
		"pod-X", "housekeeper", "/workspace/.gocdnext/outputs/abc.env",
		map[string]string{"next": "NEXT"})
	if err == nil {
		t.Fatal("want parse error, got nil")
	}
	if !strings.Contains(err.Error(), "missing `=`") {
		t.Errorf("error should mention missing `=`, got %q", err.Error())
	}
}

func TestReadOutputsFromPod_EmptyDeclaredIsNoOp(t *testing.T) {
	// Job didn't declare outputs:; the runner should skip
	// ReadOutputsFromPod entirely. If something calls it anyway
	// (defensive), return (nil, nil) without execing.
	exec := &fakePodExecutor{} // unset cmdErr; would surprise the test if called
	got, err := ReadOutputsFromPod(context.Background(), exec,
		"pod-X", "housekeeper", "/workspace/.gocdnext/outputs/abc.env", nil)
	if err != nil || got != nil {
		t.Fatalf("empty declared: want (nil, nil), got (%v, %v)", got, err)
	}
	if exec.gotCmd != nil {
		t.Errorf("exec should not have been called for empty declared, cmd=%v", exec.gotCmd)
	}
}

func TestReadOutputsFromPod_RejectsNonAbsoluteContainerPath(t *testing.T) {
	// Defence in depth: an empty/relative path is a wiring bug
	// (agent didn't compute mountPath+rel). Fail loud rather than
	// exec something nonsensical.
	exec := &fakePodExecutor{}
	_, err := ReadOutputsFromPod(context.Background(), exec,
		"pod-X", "housekeeper", "relative/path.env",
		map[string]string{"next": "NEXT"})
	if err == nil {
		t.Fatal("want error for non-absolute path, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should mention absolute, got %q", err.Error())
	}
	if exec.gotCmd != nil {
		t.Errorf("should not have execd a relative-path read, cmd=%v", exec.gotCmd)
	}
}

// Sanity-check that ReadOutputsFromPod's signature matches the
// engine.PodExecutor interface a real Kubernetes engine provides.
var _ engine.PodExecutor = (*fakePodExecutor)(nil)
