package github_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

func TestParsePushEvent_BranchPush(t *testing.T) {
	t.Parallel()

	ev, err := github.ParsePushEvent(loadFixture(t, "push_main.json"))
	if err != nil {
		t.Fatalf("ParsePushEvent: %v", err)
	}

	if got, want := ev.Ref, "refs/heads/main"; got != want {
		t.Errorf("Ref = %q, want %q", got, want)
	}
	if got, want := ev.Branch, "main"; got != want {
		t.Errorf("Branch = %q, want %q", got, want)
	}
	if ev.IsTag {
		t.Errorf("IsTag = true, want false")
	}
	if ev.Deleted {
		t.Errorf("Deleted = true, want false")
	}
	if got, want := ev.After, "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1"; got != want {
		t.Errorf("After = %q, want %q", got, want)
	}
	if got, want := ev.Repository.FullName, "gocdnext/gocdnext"; got != want {
		t.Errorf("Repository.FullName = %q, want %q", got, want)
	}
	if got, want := ev.Repository.CloneURL, "https://github.com/gocdnext/gocdnext.git"; got != want {
		t.Errorf("Repository.CloneURL = %q, want %q", got, want)
	}
	if ev.HeadCommit == nil {
		t.Fatal("HeadCommit is nil")
	}
	if got, want := ev.HeadCommit.ID, ev.After; got != want {
		t.Errorf("HeadCommit.ID = %q, want %q", got, want)
	}
	if got, want := ev.HeadCommit.Message, "feat(parser): switch pipeline config to .gocdnext/ folder pattern"; got != want {
		t.Errorf("HeadCommit.Message = %q, want %q", got, want)
	}
	if got, want := ev.HeadCommit.Author.Name, "Alice Dev"; got != want {
		t.Errorf("HeadCommit.Author.Name = %q, want %q", got, want)
	}
	wantTime, _ := time.Parse(time.RFC3339, "2026-04-17T10:15:30Z")
	if !ev.HeadCommit.Timestamp.Equal(wantTime) {
		t.Errorf("HeadCommit.Timestamp = %v, want %v", ev.HeadCommit.Timestamp, wantTime)
	}
	if got, want := len(ev.Commits), 1; got != want {
		t.Errorf("len(Commits) = %d, want %d", got, want)
	}
}

func TestParsePushEvent_Tag(t *testing.T) {
	t.Parallel()

	ev, err := github.ParsePushEvent(loadFixture(t, "push_tag.json"))
	if err != nil {
		t.Fatalf("ParsePushEvent: %v", err)
	}

	if !ev.IsTag {
		t.Errorf("IsTag = false, want true")
	}
	if got, want := ev.Tag, "v1.2.3"; got != want {
		t.Errorf("Tag = %q, want %q", got, want)
	}
	if ev.Branch != "" {
		t.Errorf("Branch = %q, want empty", ev.Branch)
	}
}

func TestParsePushEvent_BranchDeleted(t *testing.T) {
	t.Parallel()

	ev, err := github.ParsePushEvent(loadFixture(t, "push_delete.json"))
	if err != nil {
		t.Fatalf("ParsePushEvent: %v", err)
	}

	if !ev.Deleted {
		t.Errorf("Deleted = false, want true")
	}
	if got, want := ev.Branch, "feature/dead"; got != want {
		t.Errorf("Branch = %q, want %q", got, want)
	}
}

func TestParsePushEvent_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    []byte
		wantErr error
	}{
		{
			name:    "empty body",
			body:    nil,
			wantErr: github.ErrEmptyPayload,
		},
		{
			name:    "invalid json",
			body:    []byte(`{not json`),
			wantErr: github.ErrInvalidJSON,
		},
		{
			name:    "missing ref",
			body:    []byte(`{"repository":{"full_name":"x/y","clone_url":"https://x/y.git"}}`),
			wantErr: github.ErrMissingRef,
		},
		{
			name:    "missing repository",
			body:    []byte(`{"ref":"refs/heads/main"}`),
			wantErr: github.ErrMissingRepository,
		},
		{
			name:    "unsupported ref",
			body:    []byte(`{"ref":"refs/pull/1/head","repository":{"full_name":"x/y","clone_url":"https://x/y.git"}}`),
			wantErr: github.ErrUnsupportedRef,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := github.ParsePushEvent(tt.body)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadFixture %s: %v", name, err)
	}
	return b
}
