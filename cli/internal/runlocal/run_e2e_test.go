package runlocal

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dockerAvailable() bool {
	return exec.Command("docker", "version").Run() == nil
}

// End-to-end against the real local daemon: two stages, needs order,
// shared workspace between jobs (the implicit artifact flow),
// env-file secret resolution, approval auto-skip, and the failure
// path skipping later stages.
func TestRunLocal_E2E(t *testing.T) {
	if testing.Short() || !dockerAvailable() {
		t.Skip("requires a reachable docker daemon")
	}

	dir := t.TempDir()
	pipeline := filepath.Join(dir, "ci.yaml")
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("MY_TOKEN=s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pipeline, []byte(`
name: e2e
stages: [build, verify]
jobs:
  gate:
    stage: build
    approval:
      description: "skipped locally"
  produce:
    stage: build
    image: alpine:3.20
    secrets: [MY_TOKEN]
    script:
      - echo "made-by-produce token=$MY_TOKEN" > out.txt
  consume:
    stage: verify
    image: alpine:3.20
    script:
      - grep -q made-by-produce out.txt
      - echo consumed
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var out bytes.Buffer
	err := Run(ctx, &out, Options{
		File:      pipeline,
		Workspace: dir,
		EnvFile:   envFile,
	})
	if err != nil {
		t.Fatalf("Run = %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"APPROVAL GATE",      // gate skipped loudly
		"[consume] consumed", // second stage saw first stage's file
		"2 job(s) green",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	// The produced file is on the HOST workspace (mounted) — local
	// debugging artifact, also proves the mount worked.
	if b, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil || !strings.Contains(string(b), "token=s3cr3t") {
		t.Fatalf("workspace file wrong: %q err=%v", b, err)
	}
}

func TestRunLocal_E2E_FailureStopsPipeline(t *testing.T) {
	if testing.Short() || !dockerAvailable() {
		t.Skip("requires a reachable docker daemon")
	}
	dir := t.TempDir()
	pipeline := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(pipeline, []byte(`
name: e2e-fail
stages: [a, b]
jobs:
  boom:
    stage: a
    image: alpine:3.20
    script: ["echo before-boom", "exit 3"]
  never:
    stage: b
    image: alpine:3.20
    script: ["echo SHOULD-NOT-RUN"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var out bytes.Buffer
	err := Run(ctx, &out, Options{File: pipeline, Workspace: dir})
	if err == nil {
		t.Fatalf("Run = nil, want failure\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "exit 3") {
		t.Fatalf("error should carry the exit code: %v", err)
	}
	if strings.Contains(out.String(), "SHOULD-NOT-RUN") {
		t.Fatalf("stage b ran after stage a failed:\n%s", out.String())
	}
}

func TestRunLocal_MissingSecretFailsLoud(t *testing.T) {
	if testing.Short() || !dockerAvailable() {
		t.Skip("requires a reachable docker daemon")
	}
	dir := t.TempDir()
	pipeline := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(pipeline, []byte(`
name: e2e-secret
stages: [a]
jobs:
  needs-token:
    stage: a
    image: alpine:3.20
    secrets: [ABSENT_TOKEN]
    script: ["true"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var out bytes.Buffer
	err := Run(ctx, &out, Options{File: pipeline, Workspace: dir})
	if err == nil || !strings.Contains(err.Error(), "ABSENT_TOKEN") {
		t.Fatalf("err = %v, want mention of ABSENT_TOKEN", err)
	}
}
