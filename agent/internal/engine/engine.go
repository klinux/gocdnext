// Package engine defines how an agent actually executes the script
// portion of a job. Different engines map to different runtimes:
//
//   - Shell:      exec.Command against the agent's own host. Fast,
//                 zero dependencies; ignores Image and Env isolation.
//                 The dev/local default.
//   - Kubernetes: (F3.2) spawns one Pod per script with the job's
//                 declared Image, streaming logs back via the watch
//                 API.
//
// The runner (`agent/internal/runner`) owns everything outside the
// engine's responsibility — checkout, artifact download/upload, log
// masking, JobResult emission. The engine only has to "run this
// script and emit lines as you see them".
package engine

import "context"

// ScriptSpec is the contract: a pre-materialised workspace, an
// optional image (engines that don't containerise ignore this), env
// vars, and a shell script. OnLine is the callback for each stdout/
// stderr line — called from goroutines owned by the engine; the
// runner is responsible for thread-safety if it shares state.
type ScriptSpec struct {
	WorkDir string
	Image   string
	Env     map[string]string
	Script  string
	OnLine  func(stream, text string)
}

// Engine is the narrow contract: "run this, tell me when each line
// comes out, give me the exit code". Errors are reserved for
// lifecycle problems (fork/exec failure, Pod creation failure) —
// exit != 0 returns (N, nil).
type Engine interface {
	// Name identifies the engine for log/metric labels.
	Name() string

	// RunScript blocks until the script finishes or ctx is cancelled.
	// Returns the exit code. err != nil signals "could not run at
	// all" (distinct from "ran and failed").
	RunScript(ctx context.Context, spec ScriptSpec) (exitCode int, err error)
}
