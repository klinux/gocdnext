package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// mkdirAndWrite is the t.TempDir+test fixture helper — creates the
// parent directories then writes the bytes. Failure to create the
// path is the test author's bug, not the SUT's, so we return the
// error and let the caller t.Fatal.
func mkdirAndWrite(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// Compile-time guard: keep the fake honest. If the interface
// changes shape, this line breaks before any test runs.
var _ engine.PodExecutor = (*routingFakeExec)(nil)

// routingFakeExec is a PodExecutor stub that returns different
// responses depending on the command's first argv element (`find`
// vs `cat`). Lets one fake serve both calls of
// scanTestReportsFromPod (the listing pass and the per-file reads)
// without smuggling state through shared mutable fields.
type routingFakeExec struct {
	findOut   string
	findErr   error
	files     map[string][]byte // filepath → cat stdout
	catErrs   map[string]error  // filepath → cat error (overrides files)
	mu        sync.Mutex
	gotFinds  int
	gotCats   []string
	gotPod    string
	gotCont   string
	gotWorkDir string
}

func (r *routingFakeExec) Exec(_ context.Context, pod, container string, cmd []string,
	_ io.Reader, stdout, stderr io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gotPod = pod
	r.gotCont = container
	if len(cmd) == 0 {
		return errors.New("empty command")
	}
	switch cmd[0] {
	case "find":
		r.gotFinds++
		if len(cmd) >= 2 {
			r.gotWorkDir = cmd[1]
		}
		if r.findErr != nil {
			_, _ = stderr.Write([]byte("find: simulated failure"))
			return r.findErr
		}
		_, _ = stdout.Write([]byte(r.findOut))
		return nil
	case "cat":
		// cmd = ["cat", "--", "<path>"]
		if len(cmd) < 3 {
			return errors.New("cat needs path")
		}
		p := cmd[2]
		r.gotCats = append(r.gotCats, p)
		if e, ok := r.catErrs[p]; ok {
			_, _ = stderr.Write([]byte("cat error sim"))
			return e
		}
		if b, ok := r.files[p]; ok {
			_, _ = stdout.Write(b)
			return nil
		}
		return errors.New("file not found")
	default:
		return errors.New("unexpected command: " + cmd[0])
	}
}

// captureRunner builds a Runner whose Send callback records every
// AgentMessage so the test can assert what was shipped to the
// server (TestResultBatch frames in particular).
type captureRunner struct {
	*Runner
	mu       sync.Mutex
	results  []*gocdnextv1.TestResultBatch
	logLines []string
}

func newCaptureRunner(t *testing.T) *captureRunner {
	t.Helper()
	cr := &captureRunner{}
	cr.Runner = New(Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send: func(msg *gocdnextv1.AgentMessage) {
			cr.mu.Lock()
			defer cr.mu.Unlock()
			switch k := msg.Kind.(type) {
			case *gocdnextv1.AgentMessage_TestResults:
				cr.results = append(cr.results, k.TestResults)
			case *gocdnextv1.AgentMessage_Log:
				cr.logLines = append(cr.logLines, k.Log.GetText())
			}
		},
	})
	return cr
}

func assignmentWithReports(globs ...string) *gocdnextv1.JobAssignment {
	return &gocdnextv1.JobAssignment{
		RunId:       "run-1",
		JobId:       "job-1",
		Name:        "job",
		TestReports: globs,
	}
}

// TestScanTestReportsFromPod_HappyPath covers the canonical Card
// scenario: assignment declares one `**/build/test-results/test/*.xml`
// glob, find lists two matching XML files in two subprojects, and
// each cat returns a JUnit XML. The shipped TestResultBatch
// aggregates cases across both files — same contract as shared mode.
func TestScanTestReportsFromPod_HappyPath(t *testing.T) {
	workDir := "/workspace/src/abc"
	pathA := workDir + "/domain/build/test-results/test/TEST-FooTest.xml"
	pathB := workDir + "/secondary/mysql/build/test-results/test/TEST-BarTest.xml"
	other := workDir + "/build.gradle" // shouldn't match

	exec := &routingFakeExec{
		findOut: pathA + "\n" + pathB + "\n" + other + "\n",
		files: map[string][]byte{
			pathA: []byte(junitAggregate),
			pathB: []byte(junitSingle),
		},
	}
	cr := newCaptureRunner(t)
	var seq atomic.Int64
	a := assignmentWithReports("**/build/test-results/test/*.xml")

	cr.scanTestReportsFromPod(context.Background(), exec, "pod-X", "housekeeper", workDir, a, &seq)

	cr.mu.Lock()
	defer cr.mu.Unlock()

	if len(cr.results) != 1 {
		t.Fatalf("TestResultBatch frames = %d, want 1", len(cr.results))
	}
	batch := cr.results[0]
	if batch.GetRunId() != "run-1" || batch.GetJobId() != "job-1" {
		t.Errorf("batch run/job: got %q/%q", batch.GetRunId(), batch.GetJobId())
	}
	// junitAggregate has 5 cases, junitSingle has 1.
	if got := len(batch.GetResults()); got != 6 {
		t.Errorf("results = %d, want 6 (5 + 1)", got)
	}
	// Should have catted ONLY the two matching files, not build.gradle.
	if got := len(exec.gotCats); got != 2 {
		t.Errorf("cat calls = %d, want 2 (matched files only); got %v", got, exec.gotCats)
	}
	// Find called exactly once: the listing is shared across all
	// declared globs.
	if exec.gotFinds != 1 {
		t.Errorf("find calls = %d, want 1", exec.gotFinds)
	}
	if exec.gotPod != "pod-X" || exec.gotCont != "housekeeper" {
		t.Errorf("target: pod=%q cont=%q", exec.gotPod, exec.gotCont)
	}
	if exec.gotWorkDir != workDir {
		t.Errorf("find arg: workDir=%q, want %q", exec.gotWorkDir, workDir)
	}
}

// TestScanTestReportsFromPod_NoGlobsDeclared is the no-op path:
// assignment has no test_reports → nothing ever execs.
func TestScanTestReportsFromPod_NoGlobsDeclared(t *testing.T) {
	exec := &routingFakeExec{}
	cr := newCaptureRunner(t)
	var seq atomic.Int64
	a := assignmentWithReports() // empty

	cr.scanTestReportsFromPod(context.Background(), exec, "pod-X", "housekeeper", "/workspace", a, &seq)

	if exec.gotFinds != 0 {
		t.Errorf("find calls = %d on empty globs, want 0", exec.gotFinds)
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if len(cr.results) != 0 {
		t.Errorf("results = %d, want 0", len(cr.results))
	}
}

// TestScanTestReportsFromPod_FindFailureEmitsWarning surfaces the
// find error as a stderr log line for the operator. No batch is
// shipped, the job itself is not affected — tests are
// observability, not part of the contract.
func TestScanTestReportsFromPod_FindFailureEmitsWarning(t *testing.T) {
	exec := &routingFakeExec{findErr: errors.New("permission denied")}
	cr := newCaptureRunner(t)
	var seq atomic.Int64
	a := assignmentWithReports("**/*.xml")

	cr.scanTestReportsFromPod(context.Background(), exec, "pod-X", "housekeeper", "/workspace", a, &seq)

	cr.mu.Lock()
	defer cr.mu.Unlock()
	if len(cr.results) != 0 {
		t.Errorf("results = %d, want 0 on find failure", len(cr.results))
	}
	if !anyContains(cr.logLines, "list workspace files") {
		t.Errorf("expected 'list workspace files' warning, got %v", cr.logLines)
	}
}

// TestScanTestReportsFromPod_PerFileCatFailureContinues — when one
// cat fails (corrupt fs entry, race with cleanup), the other matches
// still produce results. Partial coverage beats zero.
func TestScanTestReportsFromPod_PerFileCatFailureContinues(t *testing.T) {
	workDir := "/workspace"
	good := workDir + "/g.xml"
	bad := workDir + "/b.xml"
	exec := &routingFakeExec{
		findOut: good + "\n" + bad + "\n",
		files:   map[string][]byte{good: []byte(junitSingle)},
		catErrs: map[string]error{bad: errors.New("io error")},
	}
	cr := newCaptureRunner(t)
	var seq atomic.Int64
	a := assignmentWithReports("*.xml")

	cr.scanTestReportsFromPod(context.Background(), exec, "pod-X", "housekeeper", workDir, a, &seq)

	cr.mu.Lock()
	defer cr.mu.Unlock()
	if len(cr.results) != 1 {
		t.Fatalf("batches = %d, want 1 (good file)", len(cr.results))
	}
	if got := len(cr.results[0].GetResults()); got != 1 {
		t.Errorf("results from good file = %d, want 1", got)
	}
	if !anyContains(cr.logLines, "cat /workspace/b.xml") {
		t.Errorf("expected stderr warning for bad cat, got %v", cr.logLines)
	}
}

// TestMatchPodFilesAgainstGlobs_DoubleStarRecursion is the load-
// bearing matcher test: a `**`-recursive glob (the Card pattern)
// must walk arbitrary depth. Plain filepath.Match treats `*` as
// no-separator-only and `**` as a literal directory, which would
// silently miss everything operators wrote in their YAML.
func TestMatchPodFilesAgainstGlobs_DoubleStarRecursion(t *testing.T) {
	workDir := "/workspace/src/abc"
	files := []string{
		workDir + "/domain/build/test-results/test/TEST-A.xml",
		workDir + "/secondary/mysql/build/test-results/test/TEST-B.xml",
		workDir + "/build.gradle",
		workDir + "/main/build/test-results/integrationTest/TEST-I.xml",
	}
	got := matchPodFilesAgainstGlobs(workDir, files, []string{"**/build/test-results/test/*.xml"})
	if len(got) != 2 {
		t.Errorf("got %d matches, want 2; %v", len(got), got)
	}
	for _, want := range []string{files[0], files[1]} {
		if !contains(got, want) {
			t.Errorf("missing match %q in %v", want, got)
		}
	}
	// integrationTest is at a sibling depth and shouldn't match the test/*.xml glob.
	if contains(got, files[3]) {
		t.Errorf("integrationTest path leaked into test/*.xml glob: %v", got)
	}
}

// TestMatchPodFilesAgainstGlobs_DedupesAcrossGlobs — when two
// declared globs overlap (operator copy-pasted), each file
// surfaces once. Same dedupe contract as shared-mode expandGlobs.
func TestMatchPodFilesAgainstGlobs_DedupesAcrossGlobs(t *testing.T) {
	workDir := "/w"
	files := []string{workDir + "/a.xml", workDir + "/b.xml"}
	got := matchPodFilesAgainstGlobs(workDir, files, []string{"*.xml", "*.xml", "a.xml"})
	if len(got) != 2 {
		t.Errorf("dedup failed: got %v", got)
	}
}

// TestListPodFiles_RequiresAbsoluteWorkDir guards against a caller
// passing a relative path that `find` would resolve against the
// housekeeper's CWD — surprising and exploitable. Belt + braces.
func TestListPodFiles_RequiresAbsoluteWorkDir(t *testing.T) {
	exec := &routingFakeExec{}
	_, err := listPodFiles(context.Background(), exec, "p", "c", "relative/workspace")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("want absolute-required error, got %v", err)
	}
}

// TestCatPodFile_RequiresAbsolutePath same hygiene as above for cat.
func TestCatPodFile_RequiresAbsolutePath(t *testing.T) {
	exec := &routingFakeExec{}
	_, warn := catPodFile(context.Background(), exec, "p", "c", "rel/path")
	if warn == "" || !strings.Contains(warn, "absolute") {
		t.Errorf("want absolute-required warning, got %q", warn)
	}
}

// TestExpandGlobs_DoubleStarShared exercises the SHARED-mode
// (filepath-based) expandGlobs after the doublestar refactor.
// Pre-refactor `**/x/*.xml` silently matched nothing — operators
// writing Gradle/Maven-conventional patterns saw an empty Tests
// tab and no signal as to why.
func TestExpandGlobs_DoubleStarShared(t *testing.T) {
	tmp := t.TempDir()
	// Mirror the Card layout: build artefacts under module/build/test-results/test/.
	for _, p := range []string{
		"domain/build/test-results/test/TEST-A.xml",
		"secondary/mysql/build/test-results/test/TEST-B.xml",
		"build.gradle",
	} {
		full := tmp + "/" + p
		if err := mkdirAndWrite(full, []byte("<testsuite name='x'/>")); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	matches, err := expandGlobs(tmp, []string{"**/build/test-results/test/*.xml"})
	if err != nil {
		t.Fatalf("expandGlobs: %v", err)
	}
	if len(matches) != 2 {
		t.Errorf("matches = %d (%v), want 2 — doublestar `**` recursion isn't engaged",
			len(matches), matches)
	}
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
