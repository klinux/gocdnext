package runner

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/podfs"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// fakeIsolatedUploader returns a configurable result from
// UploadFromPod. Used by the optional-wording regression test:
// when the uploader returns *podfs.PathsMissingError, PostJob's
// optional branch must route to the neutral "no files matched"
// log, NOT the alarming "failed (continuing)" log that pre-v0.14.8
// fired for every zero-match optional.
type fakeIsolatedUploader struct {
	err  error
	refs []*gocdnextv1.ArtifactRef
}

func (f *fakeIsolatedUploader) UploadFromPod(
	_ context.Context, _ engine.PodExecutor,
	_, _, _ string, _, _ string, _ []string,
) ([]*gocdnextv1.ArtifactRef, error) {
	return f.refs, f.err
}

// captureRunnerPJ — a minimal runner with a Send hook that records
// every emitted log line keyed by stream. Distinct from
// captureRunner in execute_isolated_test.go because that one
// records TestResultBatch + log msgs together; this test only
// cares about the stream→text mapping for assertion.
type captureRunnerPJ struct {
	*Runner
	mu   sync.Mutex
	out  []string // stdout text lines
	errs []string // stderr text lines
}

func newCaptureRunnerPJ(t *testing.T) *captureRunnerPJ {
	t.Helper()
	c := &captureRunnerPJ{}
	c.Runner = New(Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send: func(msg *gocdnextv1.AgentMessage) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if l := msg.GetLog(); l != nil {
				switch l.GetStream() {
				case "stdout":
					c.out = append(c.out, l.GetText())
				case "stderr":
					c.errs = append(c.errs, l.GetText())
				}
			}
		},
	})
	return c
}

// TestPostJob_OptionalZeroMatchNeutralWording is the v0.14.8
// regression cover. Pre-fix, an optional artifact glob that
// matched no files (Card's Jacoco coverage XML, screenshots a
// test didn't take, etc.) emitted `optional artifact upload
// failed (continuing): …`. The OPTIONAL contract is "if it's not
// there, no problem" — the word "failed" was alarming noise that
// scared operators reading the log. After: stdout-level neutral
// "no files matched", no stderr/warn line, no "failed" wording.
func TestPostJob_OptionalZeroMatchNeutralWording(t *testing.T) {
	cr := newCaptureRunnerPJ(t)
	uploader := &fakeIsolatedUploader{
		err: &podfs.PathsMissingError{
			Paths: []string{"build/reports/jacoco/coverage/*.xml"},
		},
	}
	a := &gocdnextv1.JobAssignment{
		RunId: "r", JobId: "j", Name: "n",
		OptionalArtifactPaths: []string{"build/reports/jacoco/coverage/*.xml"},
	}
	var seq atomic.Int64

	_, err := cr.PostJob(context.Background(), PostJobConfig{
		Uploader:      uploader,
		PodName:       "pod",
		HousekeeperCt: "housekeeper",
		PodWorkDir:    "/workspace",
	}, a, &seq)
	if err != nil {
		// Optional path should never bubble.
		t.Fatalf("PostJob bubbled error from optional path: %v", err)
	}

	cr.mu.Lock()
	defer cr.mu.Unlock()

	// Must emit a NEUTRAL stdout line naming the unmatched glob.
	var sawNeutral bool
	for _, line := range cr.out {
		if strings.Contains(line, "no files matched") &&
			strings.Contains(line, "build/reports/jacoco/coverage/*.xml") {
			sawNeutral = true
			break
		}
	}
	if !sawNeutral {
		t.Errorf("missing neutral 'no files matched' log; stdout=%v stderr=%v", cr.out, cr.errs)
	}

	// Must NOT emit any "failed" line on the optional zero-match.
	for _, line := range cr.errs {
		if strings.Contains(line, "failed") {
			t.Errorf("optional zero-match emitted alarming 'failed' wording: %q", line)
		}
	}
}

// TestPostJob_OptionalRealFailureStaysAlarming covers the
// complement: a NON-paths-missing error (real transport failure,
// agent disconnected mid-upload) keeps the alarming "failed
// (continuing)" wording, because that IS something the operator
// might want to know about even on the optional path.
func TestPostJob_OptionalRealFailureStaysAlarming(t *testing.T) {
	cr := newCaptureRunnerPJ(t)
	uploader := &fakeIsolatedUploader{err: errSimulatedNetwork}
	a := &gocdnextv1.JobAssignment{
		RunId: "r", JobId: "j", Name: "n",
		OptionalArtifactPaths: []string{"dist/app.jar"},
	}
	var seq atomic.Int64

	_, _ = cr.PostJob(context.Background(), PostJobConfig{
		Uploader:      uploader,
		PodName:       "pod",
		HousekeeperCt: "housekeeper",
		PodWorkDir:    "/workspace",
	}, a, &seq)

	cr.mu.Lock()
	defer cr.mu.Unlock()

	var sawFailedWord bool
	for _, line := range cr.errs {
		if strings.Contains(line, "failed (continuing)") {
			sawFailedWord = true
			break
		}
	}
	if !sawFailedWord {
		t.Errorf("real transport failure should keep 'failed (continuing)' wording; stderr=%v", cr.errs)
	}
}

// TestPostJob_SkipArtifacts covers artifacts.when=on_failure on a green job:
// PostJob must NOT upload artifacts (SkipArtifacts=true) even though a
// required path is declared and the uploader would have returned a ref.
// (The failure path handles the upload for that policy.)
func TestPostJob_SkipArtifacts(t *testing.T) {
	cr := newCaptureRunnerPJ(t)
	uploader := &fakeIsolatedUploader{
		refs: []*gocdnextv1.ArtifactRef{{Path: "gitleaks.sarif"}},
	}
	a := &gocdnextv1.JobAssignment{
		RunId: "r", JobId: "j", Name: "n",
		ArtifactPaths: []string{"gitleaks.sarif"},
	}
	var seq atomic.Int64

	refs, err := cr.PostJob(context.Background(), PostJobConfig{
		Uploader:      uploader,
		PodName:       "pod",
		HousekeeperCt: "housekeeper",
		PodWorkDir:    "/workspace",
		SkipArtifacts: true,
	}, a, &seq)
	if err != nil {
		t.Fatalf("PostJob returned error: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("SkipArtifacts=true still uploaded: refs=%v", refs)
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	for _, line := range cr.out {
		if strings.Contains(line, "artifact uploaded") {
			t.Errorf("SkipArtifacts=true emitted an upload log: %q", line)
		}
	}
}

var errSimulatedNetwork = errSentinel("simulated network failure")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
