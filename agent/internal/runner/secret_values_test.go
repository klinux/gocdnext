package runner

import (
	"context"
	"io"
	"log/slog"
	"testing"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// The Kubernetes engine keeps secret-bearing env out of the Pod spec
// by matching env VALUES against the job's LogMasks (#309). That only
// works if every ScriptSpec the runner builds carries the mask list —
// a spec with an empty SecretValues silently reverts to inlining
// everything, which is exactly the leak. One case per construction
// site: script task, plugin task.
func TestRunner_PropagatesLogMasksToScriptSpec(t *testing.T) {
	const secret = "ghp_abc123_never_inline"

	tests := []struct {
		name string
		task *gocdnextv1.TaskSpec
	}{
		{
			name: "script task",
			task: &gocdnextv1.TaskSpec{
				Kind: &gocdnextv1.TaskSpec_Script{Script: "echo hi"},
			},
		},
		{
			name: "plugin task",
			task: &gocdnextv1.TaskSpec{
				Kind: &gocdnextv1.TaskSpec_Plugin{Plugin: &gocdnextv1.PluginSpec{
					Image:    "ghcr.io/example/plugin:v1",
					Settings: map[string]string{"password": secret},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := &captureEngine{}
			r := New(Config{
				WorkspaceRoot: t.TempDir(),
				Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				Send:          func(*gocdnextv1.AgentMessage) {},
				Engine:        cap,
			})
			r.Execute(context.Background(), &gocdnextv1.JobAssignment{
				RunId: "run-1", JobId: "job-1", Name: "build",
				Env:      map[string]string{"GHCR_TOKEN": secret},
				LogMasks: []string{secret},
				Tasks:    []*gocdnextv1.TaskSpec{tt.task},
			})

			cap.mu.Lock()
			defer cap.mu.Unlock()
			if len(cap.specs) != 1 {
				t.Fatalf("RunScript called %d times, want 1", len(cap.specs))
			}
			got := cap.specs[0].SecretValues
			if len(got) != 1 || got[0] != secret {
				t.Errorf("ScriptSpec.SecretValues = %v, want the job's LogMasks", got)
			}
		})
	}
}
