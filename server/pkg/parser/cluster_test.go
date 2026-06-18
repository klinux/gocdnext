package parser

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParse_Cluster_AcceptsAndLifts — `cluster:` lands on
// domain.Job.Cluster and survives JSON (the definition JSONB the
// delete-guard + dispatch resolver read by the "Cluster" field).
func TestParse_Cluster_AcceptsAndLifts(t *testing.T) {
	const y = `
name: deploy
stages: [ship]
materials:
  - manual: true
jobs:
  ship:
    stage: ship
    uses: ghcr.io/klinux/gocdnext-plugin-kubectl@v1
    with:
      command: apply -k k8s/
    cluster: prod-gke
`
	p, err := ParseNamed(strings.NewReader(y), "p", "deploy")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	job := findJob(t, p, "ship")
	if job.Cluster != "prod-gke" {
		t.Fatalf("job.Cluster = %q, want prod-gke", job.Cluster)
	}
	b, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"Cluster":"prod-gke"`) {
		t.Fatalf("definition JSON missing Cluster field: %s", b)
	}
}

func TestParse_Cluster_Rejections(t *testing.T) {
	tests := []struct {
		name string
		job  string
		want string
	}{
		{
			name: "bad name (uppercase + space)",
			job: `
    stage: ship
    image: alpine
    script: ["true"]
    cluster: "Prod GKE"`,
			want: "invalid",
		},
		{
			name: "cluster + with.kubeconfig conflict",
			job: `
    stage: ship
    uses: ghcr.io/klinux/gocdnext-plugin-kubectl@v1
    with:
      command: apply -k k8s/
      kubeconfig: some-secret
    cluster: prod-gke`,
			want: "not both",
		},
		{
			name: "cluster + variables.PLUGIN_KUBECONFIG collision",
			job: `
    stage: ship
    image: alpine
    script: ["true"]
    variables:
      PLUGIN_KUBECONFIG: /tmp/x
    cluster: prod-gke`,
			want: "conflicting variables.PLUGIN_KUBECONFIG",
		},
		{
			name: "cluster + secret PLUGIN_KUBECONFIG collision",
			job: `
    stage: ship
    image: alpine
    script: ["true"]
    secrets: [PLUGIN_KUBECONFIG]
    cluster: prod-gke`,
			want: "conflicting secret",
		},
		{
			name: "cluster + id_tokens.PLUGIN_KUBECONFIG collision",
			job: `
    stage: ship
    image: alpine
    script: ["true"]
    id_tokens:
      PLUGIN_KUBECONFIG:
        aud: [https://example.com]
    cluster: prod-gke`,
			want: "conflicting id_tokens.PLUGIN_KUBECONFIG",
		},
		{
			name: "cluster + matrix dim PLUGIN_KUBECONFIG collision",
			job: `
    stage: ship
    image: alpine
    script: ["true"]
    parallel:
      matrix:
        - PLUGIN_KUBECONFIG: ["a", "b"]
    cluster: prod-gke`,
			want: "parallel.matrix dimension",
		},
		{
			name: "approval gate + cluster forbidden",
			job: `
    stage: ship
    approval:
      required: 1
      approvers: [alice]
    cluster: prod-gke`,
			want: "approval gate cannot declare",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := "name: deploy\nstages: [ship]\nmaterials:\n  - manual: true\njobs:\n  ship:" + tt.job + "\n"
			_, err := ParseNamed(strings.NewReader(y), "p", "deploy")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.want)
			}
		})
	}
}
