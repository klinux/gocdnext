package rbac

import (
	"strings"
	"testing"
)

func TestRenderDeployRBAC_Namespaced(t *testing.T) {
	out, err := RenderDeployRBAC(Options{
		Namespace:               "acme-app",
		ServiceAccountNamespace: "gocdnext",
	})
	if err != nil {
		t.Fatalf("RenderDeployRBAC: %v", err)
	}
	for _, want := range []string{
		"kind: ServiceAccount",
		"namespace: gocdnext",
		"kind: Role",
		"namespace: acme-app",
		`resources: ["jobs"]`,
		`verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]`,
		"kind: RoleBinding",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ClusterRole") {
		t.Fatalf("namespaced output contains ClusterRole:\n%s", out)
	}
}

func TestRenderDeployRBAC_ClusterWide(t *testing.T) {
	out, err := RenderDeployRBAC(Options{ClusterWide: true})
	if err != nil {
		t.Fatalf("RenderDeployRBAC: %v", err)
	}
	if !strings.Contains(out, "kind: ClusterRole") || !strings.Contains(out, "kind: ClusterRoleBinding") {
		t.Fatalf("cluster-wide output missing ClusterRole/Binding:\n%s", out)
	}
}

func TestRenderDeployRBAC_NoSecrets(t *testing.T) {
	out, err := RenderDeployRBAC(Options{Namespace: "app", IncludeSecrets: false})
	if err != nil {
		t.Fatalf("RenderDeployRBAC: %v", err)
	}
	if strings.Contains(out, `"secrets"`) {
		t.Fatalf("no-secrets output still contains secrets:\n%s", out)
	}
}

func TestRenderDeployRBAC_Validation(t *testing.T) {
	if _, err := RenderDeployRBAC(Options{}); err == nil {
		t.Fatal("missing namespace accepted")
	}
	if _, err := RenderDeployRBAC(Options{Namespace: "prod\n  annotations: {pwn: true}"}); err == nil {
		t.Fatal("YAML-injecting namespace accepted")
	}
}
