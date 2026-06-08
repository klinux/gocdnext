package runner

import (
	"context"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// runPlugin executes a plugin task — the container's own
// ENTRYPOINT handles the logic; the runner only translates the
// declared `with:` settings into `PLUGIN_*` env vars (Woodpecker
// convention) + exposes the job's existing env + network to the
// container. Returns the same (exitCode, err) shape as runScript
// so the task loop treats both uniformly.
//
// Script stays empty — engines detect that and skip the
// `sh -c "…"` wrapper, letting the image's ENTRYPOINT run as the
// author intended.
func (r *Runner) runPlugin(
	ctx context.Context,
	workDir string,
	plugin *gocdnextv1.PluginSpec,
	services servicePhase,
	jobEnv map[string]string,
	outputs outputsPaths,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) (int, error) {
	r.emitLog(a, seq, "stdout", "$ plugin "+plugin.GetImage())

	// Merge job env + PLUGIN_* env. Job env comes first so a
	// careful operator can still inject custom PLUGIN_* values
	// through variables: {} without the plugin-derived values
	// shadowing them — explicit variables: wins.
	env := make(map[string]string, len(jobEnv)+len(plugin.GetSettings()))
	for k, settingValue := range plugin.GetSettings() {
		env["PLUGIN_"+pluginEnvKey(k)] = settingValue
	}
	for k, v := range jobEnv {
		env[k] = v
	}

	return r.cfg.Engine.RunScript(ctx, engine.ScriptSpec{
		WorkDir: workDir,
		Image:   plugin.GetImage(),
		Env:     env,
		Script:  "", // empty → engine runs image's ENTRYPOINT as-is
		// Docker MUST propagate from the JobAssignment so plugins
		// that declare `docker: true` on the YAML job actually get
		// a DinD sidecar + DOCKER_HOST pointing at it. Pre-fix, this
		// field was unset and the k8s engine treated every plugin
		// task as `Docker: false`: no sidecar, no env var, the
		// plugin's `docker run` fell back to /var/run/docker.sock
		// (absent inside the plugin container) and failed with
		// "Cannot connect to the Docker daemon" miles away from the
		// cause. runScript at runner.go:422 already does this for
		// script tasks; the plugin path silently dropped it.
		Docker:          a.GetDocker(),
		Network:         services.network,
		HostAliases:     services.hostAliases,
		Resources:       assignmentResources(a),
		Profile:         a.GetProfile(),
		AgentTags:       append([]string(nil), r.cfg.AgentTags...),
		OutputsHostPath: outputs.host,
		OutputsRelPath:  outputs.rel,
		NodeSelector:    assignmentNodeSelector(a),
		Tolerations:     assignmentTolerations(a),
		OnLine: func(stream, text string) {
			r.emitLog(a, seq, stream, text)
		},
	})
}

// pluginEnvKey turns a setting key ("node-version", "targetEnv",
// "channel") into the UPPER_SNAKE form plugins expect after the
// PLUGIN_ prefix ("NODE_VERSION", "TARGET_ENV", "CHANNEL").
// Matches Woodpecker / Drone's transform so existing plugin
// images "just work".
func pluginEnvKey(k string) string {
	var b strings.Builder
	b.Grow(len(k))
	prevLower := false
	for _, r := range k {
		switch {
		case r == '-' || r == '.' || r == ' ':
			b.WriteByte('_')
			prevLower = false
		case unicode.IsUpper(r):
			// camelCase → SNAKE_CASE: insert '_' before a cap
			// that follows a lowercase letter.
			if prevLower {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			prevLower = false
		default:
			b.WriteRune(unicode.ToUpper(r))
			prevLower = unicode.IsLower(r)
		}
	}
	return b.String()
}
