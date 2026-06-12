package runlocal

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Review-round MEDIUM: explicit `variables:` beat `with:`-derived
// PLUGIN_* env — the agent's contract (runner/plugin.go applies
// plugin settings first, job env after). Local must agree.
func TestJobEnv_VariablesOverridePluginEnv(t *testing.T) {
	j := PlannedJob{
		Name:      "img",
		Variables: map[string]string{"PLUGIN_IMAGE": "explicit-wins"},
		PluginEnv: map[string]string{"PLUGIN_IMAGE": "from-with"},
	}
	env, err := jobEnv(j, map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatalf("jobEnv: %v", err)
	}
	if env["PLUGIN_IMAGE"] != "explicit-wins" {
		t.Fatalf("PLUGIN_IMAGE = %q, want explicit variables to win (agent parity)", env["PLUGIN_IMAGE"])
	}
}

// Review-round MEDIUM: the dispatch substitution phase, mirrored.
func TestJobEnv_SubstitutionParity(t *testing.T) {
	j := PlannedJob{
		Name:      "deploy",
		Secrets:   []string{"TOKEN"},
		Variables: map[string]string{"GREETING": "hi ${{ TOKEN }}"},
		PluginEnv: map[string]string{
			"PLUGIN_PASSWORD": "${{ TOKEN }}",
			"PLUGIN_TAG":      "v-${CI_COMMIT_SHORT_SHA}",
			"PLUGIN_HOME":     "literal ${HOME} stays",
		},
		MatrixKey: "GO=1.25",
	}
	ci := map[string]string{"CI_COMMIT_SHORT_SHA": "abc1234"}
	env, err := jobEnv(j, ci, map[string]string{"TOKEN": "s3cr3t"})
	if err != nil {
		t.Fatalf("jobEnv: %v", err)
	}
	if env["PLUGIN_PASSWORD"] != "s3cr3t" {
		t.Fatalf("strict ref in settings not resolved: %q", env["PLUGIN_PASSWORD"])
	}
	if env["PLUGIN_TAG"] != "v-abc1234" {
		t.Fatalf("soft ${VAR} pass missing: %q", env["PLUGIN_TAG"])
	}
	if env["PLUGIN_HOME"] != "literal ${HOME} stays" {
		t.Fatalf("unknown shell var must stay literal: %q", env["PLUGIN_HOME"])
	}
	if env["GREETING"] != "hi s3cr3t" {
		t.Fatalf("strict ref in variables not resolved: %q", env["GREETING"])
	}
	if env["GOCDNEXT_MATRIX"] != "GO=1.25" {
		t.Fatalf("GOCDNEXT_MATRIX = %q", env["GOCDNEXT_MATRIX"])
	}
	if _, leaked := env["GO"]; leaked {
		t.Fatalf("matrix dim decomposed into env — dispatch doesn't do that")
	}
}

func TestJobEnv_UnresolvedRefFailsCitingName(t *testing.T) {
	j := PlannedJob{
		Name:      "deploy",
		PluginEnv: map[string]string{"PLUGIN_KEY": "${{ SSH_DEPLOY_KEY }}"},
	}
	_, err := jobEnv(j, map[string]string{}, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "SSH_DEPLOY_KEY") {
		t.Fatalf("err = %v, want unresolved citing SSH_DEPLOY_KEY", err)
	}
}

// CI_* built-ins land after variables: a pipeline variable cannot
// shadow them (dispatch order).
func TestJobEnv_VariablesCannotShadowCIVars(t *testing.T) {
	j := PlannedJob{
		Name:      "x",
		Variables: map[string]string{"CI_BRANCH": "fake-branch"},
	}
	env, err := jobEnv(j, map[string]string{"CI_BRANCH": "main"}, nil)
	if err != nil {
		t.Fatalf("jobEnv: %v", err)
	}
	if env["CI_BRANCH"] != "main" {
		t.Fatalf("CI_BRANCH = %q — variables must not shadow built-ins", env["CI_BRANCH"])
	}
}

// Review-round MEDIUM: CI_JOB_NAME participates in the strict pass —
// the scheduler's buildCIVars carries it from the start.
func TestJobEnv_CIJobNameResolvableInVariables(t *testing.T) {
	j := PlannedJob{
		Name:      "notify",
		Variables: map[string]string{"SUBJECT": "${{ CI_JOB_NAME }} finished"},
	}
	env, err := jobEnv(j, map[string]string{}, nil)
	if err != nil {
		t.Fatalf("jobEnv: %v — CI_JOB_NAME must be strict-resolvable", err)
	}
	if env["SUBJECT"] != "notify finished" {
		t.Fatalf("SUBJECT = %q", env["SUBJECT"])
	}
}

// Review-round LOW: soft-pass precedence on collisions — a declared
// secret beats a CI built-in of the same name, like the dispatch's
// substituteShellVarsMap(settings, secrets, env).
func TestJobEnv_SoftPassSecretBeatsBuiltinOnCollision(t *testing.T) {
	j := PlannedJob{
		Name:      "x",
		Secrets:   []string{"CI_BRANCH"},
		PluginEnv: map[string]string{"PLUGIN_REF": "${CI_BRANCH}"},
	}
	env, err := jobEnv(j,
		map[string]string{"CI_BRANCH": "main"},
		map[string]string{"CI_BRANCH": "from-secret"})
	if err != nil {
		t.Fatalf("jobEnv: %v", err)
	}
	if env["PLUGIN_REF"] != "from-secret" {
		t.Fatalf("PLUGIN_REF = %q, want secret to win the soft pass (dispatch order)", env["PLUGIN_REF"])
	}
}

// Review-round LOW: short SHA is 8 chars — the scheduler's
// shortSHALen — not git's 7. Same string local and cluster.
func TestLocalCIVars_ShortSHAIsEightChars(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	run("init", "-q")
	run("commit", "--allow-empty", "-q", "-m", "x")

	vars := localCIVars(dir, "p", "manual")
	short := vars["CI_COMMIT_SHORT_SHA"]
	if len(short) != 8 {
		t.Fatalf("CI_COMMIT_SHORT_SHA = %q (%d chars), want 8 (dispatch parity)", short, len(short))
	}
	if !strings.HasPrefix(vars["CI_COMMIT_SHA"], short) {
		t.Fatalf("short %q is not a prefix of %q", short, vars["CI_COMMIT_SHA"])
	}
}
