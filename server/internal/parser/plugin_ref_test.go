package parser

import (
	"strings"
	"testing"
)

func TestResolvePluginRef(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "bare name passes through",
			in:   "gocdnext/node",
			want: "gocdnext/node",
		},
		{
			name: "tag version rewrites @ to :",
			in:   "gocdnext/node@v1",
			want: "gocdnext/node:v1",
		},
		{
			name: "semver tag",
			in:   "gocdnext/node@1.2.3",
			want: "gocdnext/node:1.2.3",
		},
		{
			name: "digest pin stays @ form",
			in:   "gocdnext/node@sha256:abcdef0123",
			want: "gocdnext/node@sha256:abcdef0123",
		},
		{
			name: "custom registry preserved",
			in:   "ghcr.io/acme/plugin@v2",
			want: "ghcr.io/acme/plugin:v2",
		},
		{
			name: "registry + digest preserved",
			in:   "ghcr.io/acme/plugin@sha256:beef",
			want: "ghcr.io/acme/plugin@sha256:beef",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePluginRef(tt.in)
			if err != nil {
				t.Fatalf("resolvePluginRef(%q): unexpected err %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolvePluginRef_RejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"whitespace":       "   \t\n",
		"embedded space":   "gocdnext/node @v1",
		"missing image":    "@v1",
		"missing version":  "gocdnext/node@",
		"only-at":          "@",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := resolvePluginRef(in)
			if err == nil {
				t.Fatalf("expected error for %q", in)
			}
		})
	}
}

func TestParse_UsesAtVersionProducesTaggedImage(t *testing.T) {
	// End-to-end: the parser turns `uses: gocdnext/node@v1` into
	// a PluginStep whose Image carries the Docker-colon tag so
	// the runner's docker run invocation pulls the pinned version
	// instead of :latest.
	y := `
stages: [build]
materials: [{manual: true}]
jobs:
  deps:
    stage: build
    uses: gocdnext/node@v1
    with:
      command: install
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	j := p.Jobs[0]
	if len(j.Tasks) != 1 || j.Tasks[0].Plugin == nil {
		t.Fatalf("expected single plugin task, got %+v", j.Tasks)
	}
	if j.Tasks[0].Plugin.Image != "gocdnext/node:v1" {
		t.Errorf("image = %q, want gocdnext/node:v1", j.Tasks[0].Plugin.Image)
	}
}

func TestParse_UsesAtDigestPinsBlob(t *testing.T) {
	y := `
stages: [build]
materials: [{manual: true}]
jobs:
  deps:
    stage: build
    uses: gocdnext/node@sha256:abc123
    with:
      command: install
`
	p, err := Parse(strings.NewReader(y), "p", "n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := p.Jobs[0].Tasks[0].Plugin.Image; got != "gocdnext/node@sha256:abc123" {
		t.Errorf("image = %q, want gocdnext/node@sha256:abc123 (digest stays @)", got)
	}
}

func TestParse_UsesRejectsEmptyVersion(t *testing.T) {
	y := `
stages: [build]
materials: [{manual: true}]
jobs:
  deps:
    stage: build
    uses: gocdnext/node@
    with:
      command: install
`
	if _, err := Parse(strings.NewReader(y), "p", "n"); err == nil {
		t.Fatal("expected parse error for empty version suffix")
	}
}
