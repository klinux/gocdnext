package runner

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

func TestShouldUploadArtifacts(t *testing.T) {
	tests := []struct {
		name       string
		when       string
		taskFailed bool
		want       bool
	}{
		{"default success uploads", "", false, true},
		{"default failure skips", "", true, false},
		{"on_success + success uploads", "on_success", false, true},
		{"on_success + failure skips", "on_success", true, false},
		{"on_failure + success skips", "on_failure", false, false},
		{"on_failure + failure uploads", "on_failure", true, true},
		{"always + success uploads", "always", false, true},
		{"always + failure uploads", "always", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldUploadArtifacts(tt.when, tt.taskFailed); got != tt.want {
				t.Fatalf("shouldUploadArtifacts(%q, %v) = %v, want %v", tt.when, tt.taskFailed, got, tt.want)
			}
		})
	}
}

// fakeSharedUploader is an internal ArtifactUploader that records whether it was
// asked to upload. (runner_test.go's fakeUploader lives in the external test
// package, out of reach for these unexported-method tests.)
type fakeSharedUploader struct {
	calls int
	refs  []*gocdnextv1.ArtifactRef
}

func (f *fakeSharedUploader) Upload(_ context.Context, _, _, _ string, paths []string) ([]*gocdnextv1.ArtifactRef, error) {
	f.calls++
	return f.refs, nil
}

func quietRunner(uploader ArtifactUploader, iso IsolatedUploader) *Runner {
	return New(Config{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send:             func(*gocdnextv1.AgentMessage) {},
		Uploader:         uploader,
		IsolatedUploader: iso,
	})
}

// TestUploadArtifactsOnFailure locks the shared-mode failure helper that ALL
// three failure branches call — task error, non-zero task exit, and the
// coverage fail_under gate. on_failure/always ship the SARIF on a red job;
// on_success (the default) does not.
func TestUploadArtifactsOnFailure(t *testing.T) {
	tests := []struct {
		name       string
		when       string
		wantUpload bool
	}{
		{"default skips on failure", "", false},
		{"on_success skips on failure", "on_success", false},
		{"on_failure uploads", "on_failure", true},
		{"always uploads", "always", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &fakeSharedUploader{refs: []*gocdnextv1.ArtifactRef{{Path: "x.sarif"}}}
			r := quietRunner(up, nil)
			a := &gocdnextv1.JobAssignment{
				RunId: "r", JobId: "j", Name: "n",
				ArtifactPaths: []string{"x.sarif"}, ArtifactsWhen: tt.when,
			}
			var seq atomic.Int64
			refs := r.uploadArtifactsOnFailure(context.Background(), t.TempDir(), a, &seq)
			if got := up.calls > 0; got != tt.wantUpload {
				t.Fatalf("when=%q: upload called=%v, want %v", tt.when, got, tt.wantUpload)
			}
			if tt.wantUpload && len(refs) == 0 {
				t.Fatalf("when=%q: expected refs, got none", tt.when)
			}
			if !tt.wantUpload && len(refs) != 0 {
				t.Fatalf("when=%q: expected no refs, got %v", tt.when, refs)
			}
		})
	}

	// No declared artifacts → never uploads, even with always.
	up := &fakeSharedUploader{}
	a := &gocdnextv1.JobAssignment{RunId: "r", JobId: "j", Name: "n", ArtifactsWhen: "always"}
	var seq atomic.Int64
	if refs := quietRunner(up, nil).uploadArtifactsOnFailure(context.Background(), t.TempDir(), a, &seq); len(refs) != 0 || up.calls != 0 {
		t.Fatalf("no artifacts declared: want no upload, got refs=%v calls=%d", refs, up.calls)
	}
}

// TestPostJobArtifactsOnFailure locks the isolated-mode counterpart (used by
// the task-failure and fail_under branches). Reuses fakeIsolatedUploader from
// postjob_test.go; a nil PodExecutor is fine — the fake ignores it.
func TestPostJobArtifactsOnFailure(t *testing.T) {
	tests := []struct {
		name       string
		when       string
		wantUpload bool
	}{
		{"on_success skips", "on_success", false},
		{"on_failure uploads", "on_failure", true},
		{"always uploads", "always", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iso := &fakeIsolatedUploader{refs: []*gocdnextv1.ArtifactRef{{Path: "x.sarif"}}}
			r := quietRunner(nil, iso)
			a := &gocdnextv1.JobAssignment{
				RunId: "r", JobId: "j", Name: "n",
				ArtifactPaths: []string{"x.sarif"}, ArtifactsWhen: tt.when,
			}
			var seq atomic.Int64
			refs := r.postJobArtifactsOnFailure(context.Background(), nil, "pod", "/workspace", a, &seq)
			if tt.wantUpload && len(refs) == 0 {
				t.Fatalf("when=%q: expected refs, got none", tt.when)
			}
			if !tt.wantUpload && len(refs) != 0 {
				t.Fatalf("when=%q: expected no refs, got %v", tt.when, refs)
			}
		})
	}
}
