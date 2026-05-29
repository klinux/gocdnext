package main

import (
	"strings"
	"testing"
)

// TestResolveWorkspaceRoot locks the inference matrix that v0.4.14
// settled: the agent picks the right path on its own from the
// engine choice, so an operator never has to set two env vars that
// must agree.
func TestResolveWorkspaceRoot(t *testing.T) {
	tests := []struct {
		name    string
		envs    map[string]string
		want    string
		wantErr string // substring; empty = expect success
	}{
		{
			name: "shell engine (default) leaves WorkspaceRoot empty so runner uses /tmp",
			envs: map[string]string{},
			want: "",
		},
		{
			name: "explicit shell engine — same behaviour",
			envs: map[string]string{"GOCDNEXT_AGENT_ENGINE": "shell"},
			want: "",
		},
		{
			name: "docker engine — agent shares host fs through docker.sock; /tmp fine",
			envs: map[string]string{"GOCDNEXT_AGENT_ENGINE": "docker"},
			want: "",
		},
		{
			name: "kubernetes engine + workspace path → infer to match PVC mount",
			envs: map[string]string{
				"GOCDNEXT_AGENT_ENGINE":       "kubernetes",
				"GOCDNEXT_K8S_WORKSPACE_PATH": "/workspace",
			},
			want: "/workspace",
		},
		{
			name: "kubernetes engine + custom workspace path",
			envs: map[string]string{
				"GOCDNEXT_AGENT_ENGINE":       "kubernetes",
				"GOCDNEXT_K8S_WORKSPACE_PATH": "/var/lib/gocdnext",
			},
			want: "/var/lib/gocdnext",
		},
		{
			name: "kubernetes engine without workspace path → fail loud at boot",
			envs: map[string]string{
				"GOCDNEXT_AGENT_ENGINE": "kubernetes",
			},
			wantErr: "GOCDNEXT_K8S_WORKSPACE_PATH",
		},
		{
			name: "explicit override wins regardless of engine",
			envs: map[string]string{
				"GOCDNEXT_AGENT_ENGINE":       "kubernetes",
				"GOCDNEXT_K8S_WORKSPACE_PATH": "/workspace",
				"GOCDNEXT_WORKSPACE_ROOT":     "/custom/path",
			},
			want: "/custom/path",
		},
		{
			name: "override beats unset workspace path (operator knows what they're doing)",
			envs: map[string]string{
				"GOCDNEXT_AGENT_ENGINE":   "kubernetes",
				"GOCDNEXT_WORKSPACE_ROOT": "/manual/mount",
			},
			want: "/manual/mount",
		},
		{
			name: "whitespace-only override is treated as unset (defensive trim)",
			envs: map[string]string{
				"GOCDNEXT_AGENT_ENGINE":       "kubernetes",
				"GOCDNEXT_K8S_WORKSPACE_PATH": "/workspace",
				"GOCDNEXT_WORKSPACE_ROOT":     "   ",
			},
			want: "/workspace",
		},
		{
			name: "engine name is case-insensitive (matches buildEngine)",
			envs: map[string]string{
				"GOCDNEXT_AGENT_ENGINE":       "Kubernetes",
				"GOCDNEXT_K8S_WORKSPACE_PATH": "/workspace",
			},
			want: "/workspace",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv unsets at end of test — no leak across cases.
			for _, k := range []string{
				"GOCDNEXT_AGENT_ENGINE",
				"GOCDNEXT_K8S_WORKSPACE_PATH",
				"GOCDNEXT_WORKSPACE_ROOT",
			} {
				if v, ok := tc.envs[k]; ok {
					t.Setenv(k, v)
				} else {
					t.Setenv(k, "")
				}
			}
			got, err := resolveWorkspaceRoot()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil; out=%q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
