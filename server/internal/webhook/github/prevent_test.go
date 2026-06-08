package github_test

import (
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

func TestParsePullRequestEvent_Opened(t *testing.T) {
	ev, err := github.ParsePullRequestEvent(loadFixture(t, "pr_opened.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Action != "opened" {
		t.Errorf("action = %q", ev.Action)
	}
	if ev.Number != 42 {
		t.Errorf("number = %d", ev.Number)
	}
	if ev.HeadSHA != "9f7c3d2e1b8a5f6c4e0d7a9b1c3d5e7f9a0b2c4d" {
		t.Errorf("head sha = %q", ev.HeadSHA)
	}
	if ev.HeadRef != "feat/gizmo" {
		t.Errorf("head ref = %q", ev.HeadRef)
	}
	if ev.BaseRef != "main" {
		t.Errorf("base ref = %q", ev.BaseRef)
	}
	if ev.Author != "kleber" {
		t.Errorf("author = %q", ev.Author)
	}
	if ev.HTMLURL == "" {
		t.Error("html url missing")
	}
	if ev.Repository.CloneURL != "https://github.com/org/demo.git" {
		t.Errorf("clone url = %q", ev.Repository.CloneURL)
	}
	// Labels: GitHub case-insensitive `Hotfix` and `hotfix` collapse
	// to one lowercased entry; `needs-review` stays distinct. Order
	// preserved minus duplicates. Downstream YAML
	// (`quorum_by_label`) matches against the lowercased value.
	wantLabels := []string{"hotfix", "needs-review"}
	if len(ev.Labels) != len(wantLabels) {
		t.Fatalf("labels = %v, want %v", ev.Labels, wantLabels)
	}
	for i, want := range wantLabels {
		if ev.Labels[i] != want {
			t.Errorf("labels[%d] = %q, want %q", i, ev.Labels[i], want)
		}
	}
}

func TestParsePullRequestEvent_EmptyLabels(t *testing.T) {
	// PR with no labels — Labels stays nil, not a length-0
	// non-nil slice, so downstream nil-checks (`if len() > 0`) +
	// JSON marshalling (`omitempty`) both behave.
	body := []byte(`{
		"action":"opened","number":1,
		"pull_request":{
			"html_url":"x","title":"t","updated_at":"2026-01-01T00:00:00Z",
			"head":{"ref":"f","sha":"abc"},
			"base":{"ref":"main","sha":"def"}
		},
		"repository":{"clone_url":"https://github.com/org/x.git","full_name":"org/x"}
	}`)
	ev, err := github.ParsePullRequestEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Labels != nil {
		t.Errorf("Labels = %v, want nil for PR without labels", ev.Labels)
	}
}

func TestParsePullRequestEvent_TriggerableActions(t *testing.T) {
	tests := []struct {
		action string
		want   bool
	}{
		{"opened", true},
		{"synchronize", true},
		{"reopened", true},
		{"closed", false},
		{"labeled", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			ev := github.PullRequestEvent{Action: tt.action}
			if got := ev.IsTriggerableAction(); got != tt.want {
				t.Errorf("IsTriggerableAction(%q) = %v, want %v", tt.action, got, tt.want)
			}
		})
	}
}

func TestParsePullRequestEvent_Rejects(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"empty", []byte{}},
		{"malformed", []byte(`{`)},
		{"missing action", []byte(`{"number":1,"pull_request":{"head":{"sha":"a","ref":"b"},"base":{"sha":"c","ref":"d"}},"repository":{"clone_url":"x"}}`)},
		{"missing pr", []byte(`{"action":"opened","number":1,"repository":{"clone_url":"x"}}`)},
		{"missing repo", []byte(`{"action":"opened","number":1,"pull_request":{"head":{"sha":"a","ref":"b"},"base":{"sha":"c","ref":"d"}}}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := github.ParsePullRequestEvent(tt.body); err == nil {
				t.Error("expected error")
			}
		})
	}
}
