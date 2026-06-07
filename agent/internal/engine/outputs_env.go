package engine

// withOutputsEnv returns a fresh copy of env with OutputsEnvName set
// to the given path. The caller's map is never mutated — engines
// share ScriptSpec.Env with the runner that built it, and stamping
// in place would persist into the cached assignment for retries.
//
// Each engine calls this once inside RunScript with the path the
// SCRIPT will actually see:
//   - Shell: spec.OutputsHostPath (host path, scripts run with cmd.Dir = WorkDir)
//   - Docker (containerized): ContainerWorkspaceMount + "/" + OutputsRelPath
//   - Docker fallback to Shell: skipped (Shell handles via OutputsHostPath)
//   - Kubernetes: ContainerWorkspaceMount + "/" + OutputsRelPath
//
// nil env is fine — returns a single-entry map.
func withOutputsEnv(env map[string]string, path string) map[string]string {
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out[OutputsEnvName] = path
	return out
}
