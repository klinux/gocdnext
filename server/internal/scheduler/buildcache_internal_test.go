package scheduler

import (
	"testing"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

func TestImageRepoBasename(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"ghcr.io/klinux/gocdnext-plugin-gradle@sha256:abc", "gocdnext-plugin-gradle"},
		{"ghcr.io/klinux/gocdnext-plugin-gradle:v1", "gocdnext-plugin-gradle"},
		{"ghcr.io/klinux/gocdnext-plugin-gradle", "gocdnext-plugin-gradle"},
		{"host:5000/klinux/gocdnext-plugin-gradle:v1", "gocdnext-plugin-gradle"},
		{"img.cora.tools/gocdnext-plugin-gradle:latest", "gocdnext-plugin-gradle"},
		// Look-alikes must NOT normalise to the real repo.
		{"ghcr.io/evil/gocdnext-plugin-gradle-backdoor:v1", "gocdnext-plugin-gradle-backdoor"},
		{"gocdnext-plugin-gradlex", "gocdnext-plugin-gradlex"},
	}
	for _, tt := range tests {
		if got := imageRepoBasename(tt.in); got != tt.want {
			t.Errorf("imageRepoBasename(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUsesGradlePlugin(t *testing.T) {
	plugin := func(img string) *gocdnextv1.TaskSpec {
		return &gocdnextv1.TaskSpec{Kind: &gocdnextv1.TaskSpec_Plugin{
			Plugin: &gocdnextv1.PluginSpec{Image: img},
		}}
	}
	script := &gocdnextv1.TaskSpec{Kind: &gocdnextv1.TaskSpec_Script{Script: "echo hi"}}

	tests := []struct {
		name  string
		tasks []*gocdnextv1.TaskSpec
		want  bool
	}{
		{"real gradle plugin", []*gocdnextv1.TaskSpec{plugin("ghcr.io/klinux/gocdnext-plugin-gradle@v1")}, true},
		{"gradle among others", []*gocdnextv1.TaskSpec{script, plugin("ghcr.io/klinux/gocdnext-plugin-maven:v1"), plugin("ghcr.io/klinux/gocdnext-plugin-gradle:v1")}, true},
		{"backdoor look-alike rejected", []*gocdnextv1.TaskSpec{plugin("ghcr.io/evil/gocdnext-plugin-gradle-backdoor:v1")}, false},
		{"other plugin", []*gocdnextv1.TaskSpec{plugin("ghcr.io/klinux/gocdnext-plugin-maven:v1")}, false},
		{"script only", []*gocdnextv1.TaskSpec{script}, false},
		{"no tasks", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &gocdnextv1.JobAssignment{Tasks: tt.tasks}
			if got := usesGradlePlugin(a); got != tt.want {
				t.Errorf("usesGradlePlugin = %v, want %v", got, tt.want)
			}
		})
	}
}
