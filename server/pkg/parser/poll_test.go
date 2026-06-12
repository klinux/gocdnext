package parser

import (
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

func TestParse_GitPollInterval(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    time.Duration
		wantErr string
	}{
		{
			name: "unset leaves poll disabled (zero duration)",
			yaml: baseYAMLWithGit(""),
			want: 0,
		},
		{
			name: "5m parses to 5 minutes",
			yaml: baseYAMLWithGit(`poll_interval: "5m"`),
			want: 5 * time.Minute,
		},
		{
			name: "1h30m parses to 90 minutes",
			yaml: baseYAMLWithGit(`poll_interval: "1h30m"`),
			want: 90 * time.Minute,
		},
		{
			name:    "too short rejected",
			yaml:    baseYAMLWithGit(`poll_interval: "30s"`),
			wantErr: "poll_interval",
		},
		{
			name:    "too long rejected",
			yaml:    baseYAMLWithGit(`poll_interval: "48h"`),
			wantErr: "poll_interval",
		},
		{
			name:    "invalid format rejected",
			yaml:    baseYAMLWithGit(`poll_interval: "5 minutes"`),
			wantErr: "poll_interval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParseNamed(strings.NewReader(tt.yaml), "proj-1", "p")
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q missing substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(p.Materials) == 0 || p.Materials[0].Git == nil {
				t.Fatalf("expected a git material")
			}
			if got := p.Materials[0].Git.PollInterval; got != tt.want {
				t.Errorf("PollInterval: want %s, got %s", tt.want, got)
			}
			if p.Materials[0].Type != domain.MaterialGit {
				t.Errorf("material type: want git")
			}
		})
	}
}

func baseYAMLWithGit(pollLine string) string {
	return `
materials:
  - git:
      url: https://github.com/org/repo
      branch: main
      ` + pollLine + `

stages: [build]

jobs:
  build:
    stage: build
    image: alpine
    script:
      - echo hi
`
}
