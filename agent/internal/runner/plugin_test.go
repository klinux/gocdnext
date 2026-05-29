package runner

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// captureEngine records each RunScript call so tests can assert on
// the materialised ScriptSpec without actually executing anything.
// The agent's runner depends on engine.Engine, not a concrete impl,
// so this stays free of docker / k8s setup.
type captureEngine struct {
	mu    sync.Mutex
	specs []engine.ScriptSpec
}

func (c *captureEngine) Name() string { return "capture" }

func (c *captureEngine) RunScript(_ context.Context, spec engine.ScriptSpec) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.specs = append(c.specs, spec)
	return 0, nil
}

// TestRunPlugin_PropagatesDockerFlag is the regression cover for the
// v0.4.9 fix: a job declaring `docker: true` on its YAML must reach
// the plugin task's ScriptSpec so the engine wires DinD + DOCKER_HOST.
// Pre-fix, the field was unset and `docker run` inside the plugin
// fell back to /var/run/docker.sock (absent inside the plugin
// container) with the misleading "Cannot connect to the Docker
// daemon" error.
func TestRunPlugin_PropagatesDockerFlag(t *testing.T) {
	tests := []struct {
		name      string
		assignDoc bool
		want      bool
	}{
		{"docker=true on the job reaches the plugin spec", true, true},
		{"docker=false leaves the spec unchanged (no sidecar)", false, false},
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
			a := &gocdnextv1.JobAssignment{
				RunId: "run-1", JobId: "job-1", Name: "buildx",
				Docker: tt.assignDoc,
				Tasks: []*gocdnextv1.TaskSpec{{
					Kind: &gocdnextv1.TaskSpec_Plugin{Plugin: &gocdnextv1.PluginSpec{
						Image:    "ghcr.io/example/plugin-buildx:v1",
						Settings: map[string]string{"image": "img.example.com/app"},
					}},
				}},
			}
			r.Execute(context.Background(), a)

			cap.mu.Lock()
			defer cap.mu.Unlock()
			if len(cap.specs) != 1 {
				t.Fatalf("RunScript called %d times, want 1", len(cap.specs))
			}
			if cap.specs[0].Docker != tt.want {
				t.Errorf("ScriptSpec.Docker = %v, want %v", cap.specs[0].Docker, tt.want)
			}
		})
	}
}

func TestPluginEnvKey_NamingConventions(t *testing.T) {
	// Plugin settings hit the container as PLUGIN_<UPPER_SNAKE>
	// env vars — the Woodpecker/Drone convention every existing
	// plugin image reads. Verify the key-transform covers the
	// shapes operators write: kebab-case, camelCase, dotted.
	cases := []struct {
		in, want string
	}{
		{"command", "COMMAND"},
		{"node-version", "NODE_VERSION"},
		{"targetEnv", "TARGET_ENV"},
		{"channel.name", "CHANNEL_NAME"},
		{"API_KEY", "API_KEY"},                 // already upper snake
		{"do-the-thing-v2", "DO_THE_THING_V2"}, // digits stay
	}
	for _, c := range cases {
		if got := pluginEnvKey(c.in); got != c.want {
			t.Errorf("pluginEnvKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
