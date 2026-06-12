package runlocal

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gocdnext/gocdnext/server/pkg/parser"
)

// Options configure one local run.
type Options struct {
	File      string // pipeline YAML path (required)
	Workspace string // host dir mounted at /workspace; default "."
	Event     string // synthesized CI_CAUSE: push | pull_request | manual
	OnlyJob   string // run a single job (and nothing else) when set
	EnvFile   string // KEY=VALUE file resolving `secrets:` (and extra env)
}

// Run executes the pipeline at opts.File against the local Docker
// daemon. Returns an error when any job fails — the CLI maps it to
// exit 1.
func Run(ctx context.Context, w io.Writer, opts Options) error {
	if opts.Workspace == "" {
		opts.Workspace = "."
	}
	ws, err := filepath.Abs(opts.Workspace)
	if err != nil {
		return err
	}
	if opts.Event == "" {
		opts.Event = "manual"
	}

	fh, err := os.Open(opts.File)
	if err != nil {
		return err
	}
	base := filepath.Base(opts.File)
	// Fallback name strips the extension — same contract as apply /
	// LoadFolder, so CI_PIPELINE_NAME never reads "ci.yaml".
	fallback := strings.TrimSuffix(base, filepath.Ext(base))
	p, err := parser.ParseNamed(fh, "local", fallback)
	_ = fh.Close()
	if err != nil {
		return fmt.Errorf("parse %s: %w", opts.File, err)
	}
	plan, err := Build(p, opts.OnlyJob)
	if err != nil {
		return err
	}

	secretsEnv, err := loadEnvFile(opts.EnvFile)
	if err != nil {
		return err
	}
	ciVars := localCIVars(ws, p.Name, opts.Event)

	d := newDocker(w)
	if err := d.check(ctx); err != nil {
		return err
	}

	// One network per run so services resolve by name, like the
	// cluster's job-scoped network. Suffix from the pid keeps
	// concurrent local runs apart without needing randomness.
	network := fmt.Sprintf("gocdnext-local-%d", os.Getpid())
	if err := d.networkCreate(ctx, network); err != nil {
		return err
	}
	defer d.networkRemove(network)

	var serviceIDs []string
	defer func() {
		for _, id := range serviceIDs {
			d.stopContainer(id)
		}
	}()
	for _, svc := range plan.Services {
		id, err := d.startService(ctx, network, svc)
		if err != nil {
			return err
		}
		serviceIDs = append(serviceIDs, id)
		fmt.Fprintf(w, "==> service %s up (%s)\n", svc.Name, svc.Image)
	}

	start := time.Now()
	ran := 0
	for _, stage := range plan.Stages {
		fmt.Fprintf(w, "==> stage %s\n", stage.Name)
		for _, j := range stage.Jobs {
			if j.Approval {
				fmt.Fprintf(w, "[%s] APPROVAL GATE — auto-skipped in run-local (a real run parks here until approved)\n", j.Name)
				continue
			}
			env, err := jobEnv(j, ciVars, secretsEnv)
			if err != nil {
				return err
			}
			ran++
			code, err := d.runJob(ctx, j, ws, network, env)
			if err != nil {
				return fmt.Errorf("job %s: %w", j.Name, err)
			}
			if code != 0 {
				fmt.Fprintf(w, "[%s] FAILED (exit %d)\n", j.Name, code)
				// Same semantics as the scheduler: a failed stage
				// skips everything after it.
				fmt.Fprintf(w, "==> stage %s failed — skipping remaining stages\n", stage.Name)
				return fmt.Errorf("job %s failed (exit %d)", j.Name, code)
			}
			fmt.Fprintf(w, "[%s] OK\n", j.Name)
		}
	}
	fmt.Fprintf(w, "==> %d job(s) green in %s\n", ran, time.Since(start).Round(time.Second))
	return nil
}

// jobEnv assembles the container env mirroring the dispatch order
// (scheduler/assignment.go): variables → GOCDNEXT_MATRIX → declared
// secrets (same-name secret beats a variable) → CI_* built-ins
// (land AFTER variables, so `variables:` can never shadow CI_*) →
// strict `${{ NAME }}` substitution on env values → plugin settings
// substituted (strict + soft `${VAR}` pass) and overlaid FIRST so
// the resolved env wins on collisions, exactly like the agent's
// merge. A declared secret missing from the env-file fails LOUD.
func jobEnv(j PlannedJob, ciVars, secretsEnv map[string]string) (map[string]string, error) {
	env := make(map[string]string, len(ciVars)+len(j.Variables)+len(j.PluginEnv)+len(j.Secrets)+2)
	for k, v := range j.Variables {
		env[k] = v
	}
	if j.MatrixKey != "" {
		env["GOCDNEXT_MATRIX"] = j.MatrixKey
	}

	secrets := make(map[string]string, len(j.Secrets))
	var missing []string
	for _, name := range j.Secrets {
		v, ok := secretsEnv[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		secrets[name] = v
		env[name] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("job %s declares secrets not in --env-file: %s",
			j.Name, strings.Join(missing, ", "))
	}

	// CI_JOB_NAME joins the built-ins BEFORE the strict pass — the
	// scheduler's buildCIVars carries it from the start, so
	// `variables: SUBJECT: "${{ CI_JOB_NAME }}"` must resolve here
	// exactly like in the cluster.
	ciVarsJob := make(map[string]string, len(ciVars)+1)
	for k, v := range ciVars {
		ciVarsJob[k] = v
	}
	ciVarsJob["CI_JOB_NAME"] = j.Name
	for k, v := range ciVarsJob {
		env[k] = v
	}

	// Strict `${{ NAME }}` pass over env values, sources matching
	// the dispatch: secrets first, CI built-ins after.
	for k, v := range env {
		resolved, err := substituteRefs(v, secrets, ciVarsJob)
		if err != nil {
			return nil, fmt.Errorf("job %s env %s: %w", j.Name, k, err)
		}
		env[k] = resolved
	}

	if len(j.PluginEnv) > 0 {
		merged := make(map[string]string, len(j.PluginEnv)+len(env))
		for k, raw := range j.PluginEnv {
			// Settings resolve against secrets + resolved env + CI
			// vars (strict), then the soft `${VAR}` pass — same two
			// phases as dispatch; a literal `${HOME}` stays literal.
			resolved, err := substituteRefs(raw, secrets, env, ciVarsJob)
			if err != nil {
				return nil, fmt.Errorf("job %s with %s: %w", j.Name, k, err)
			}
			// Soft-pass sources mirror the dispatch exactly:
			// (secrets, env) — a declared secret colliding with a
			// built-in name wins the `${VAR}` resolution
			// (assignment.go substituteShellVarsMap order).
			merged[k] = substituteShellVars(resolved, secrets, env)
		}
		// Env overlays plugin settings — the agent merges PLUGIN_*
		// first and the assignment env after, so explicit
		// `variables: PLUGIN_FOO` wins.
		for k, v := range env {
			merged[k] = v
		}
		return merged, nil
	}
	return env, nil
}

// localCIVars synthesizes the cluster's CI_* contract from the local
// git checkout. Missing git (not a repo, no commits) leaves the
// commit/branch vars unset — same omission contract as the
// scheduler, so ${CI_COMMIT_SHORT_SHA} stays literal and visible
// instead of silently empty.
func localCIVars(workspace, pipeline, event string) map[string]string {
	vars := map[string]string{
		"CI":               "true",
		"GOCDNEXT":         "true",
		"GOCDNEXT_LOCAL":   "true",
		"CI_RUN_COUNTER":   "0",
		"CI_PIPELINE_NAME": pipeline,
		"CI_CAUSE":         event,
	}
	if sha := gitOut(workspace, "rev-parse", "HEAD"); sha != "" {
		vars["CI_COMMIT_SHA"] = sha
		// 8 chars — the dispatch contract (scheduler shortSHALen),
		// NOT git's default 7: a `tag: ${CI_COMMIT_SHORT_SHA}` must
		// produce the same string local and cluster.
		if len(sha) >= 8 {
			vars["CI_COMMIT_SHORT_SHA"] = sha[:8]
		}
	}
	if branch := gitOut(workspace, "rev-parse", "--abbrev-ref", "HEAD"); branch != "" && branch != "HEAD" {
		vars["CI_BRANCH"] = branch
	}
	return vars
}

func gitOut(dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// loadEnvFile parses a KEY=VALUE file (#-comments and blank lines
// ignored). Empty path = empty map.
func loadEnvFile(path string) (map[string]string, error) {
	env := map[string]string{}
	if path == "" {
		return env, nil
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("env-file: %w", err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		k, v, ok := strings.Cut(raw, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("env-file:%d: expected KEY=VALUE, got %q", line, raw)
		}
		env[strings.TrimSpace(k)] = v
	}
	return env, sc.Err()
}
