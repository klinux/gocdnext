package github_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/scm/github"
)

type entry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	Size        int64  `json:"size"`
	Encoding    string `json:"encoding"`
	Content     string `json:"content"`
	DownloadURL string `json:"download_url"`
}

func inline(name, body string) entry {
	return entry{
		Name:     name,
		Path:     ".gocdnext/" + name,
		Type:     "file",
		Size:     int64(len(body)),
		Encoding: "base64",
		Content:  base64.StdEncoding.EncodeToString([]byte(body)),
	}
}

func TestParseRepoURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in            string
		owner, repo   string
		expectErr     bool
	}{
		{"https://github.com/org/repo", "org", "repo", false},
		{"https://github.com/org/repo.git", "org", "repo", false},
		{"https://github.com/org/repo/", "org", "repo", false},
		{"git@github.com:org/repo.git", "org", "repo", false},
		{"https://github.com/", "", "", true},
		{"", "", "", true},
		{"not-a-url", "", "", true},
	}
	for _, c := range cases {
		owner, repo, err := github.ParseRepoURL(c.in)
		if c.expectErr {
			if err == nil {
				t.Errorf("in=%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("in=%q: unexpected err: %v", c.in, err)
			continue
		}
		if owner != c.owner || repo != c.repo {
			t.Errorf("in=%q: got (%q, %q), want (%q, %q)", c.in, owner, repo, c.owner, c.repo)
		}
	}
}

func TestFetchGocdnextFolder_ReturnsInlineYAMLOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/org/repo/contents/.gocdnext" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("ref") != "abc123" {
			t.Errorf("ref = %q", r.URL.Query().Get("ref"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]entry{
			inline("build.yaml", "name: build\n"),
			inline("deploy.yml", "name: deploy\n"),
			inline("README.md", "ignored"),
			{Name: "subdir", Type: "dir"},
		})
	}))
	defer srv.Close()

	got, err := github.FetchGocdnextFolder(context.Background(), srv.Client(), github.Config{
		APIBase: srv.URL, Owner: "org", Repo: "repo",
	}, "abc123")
	if err != nil {
		t.Fatalf("FetchGocdnextFolder: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("files = %d, want 2 (yaml+yml only)", len(got))
	}
	names := []string{got[0].Name, got[1].Name}
	if !(names[0] == "build.yaml" && names[1] == "deploy.yml") {
		t.Fatalf("names = %v", names)
	}
	if !strings.Contains(got[0].Content, "name: build") {
		t.Fatalf("content[0] = %q", got[0].Content)
	}
}

func TestFetchGocdnextFolder_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	_, err := github.FetchGocdnextFolder(context.Background(), srv.Client(), github.Config{
		APIBase: srv.URL, Owner: "org", Repo: "repo",
	}, "main")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not-found", err)
	}
}

func TestFetchGocdnextFolder_SendsAuthHeaderWhenTokenSet(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]entry{})
	}))
	defer srv.Close()

	_, err := github.FetchGocdnextFolder(context.Background(), srv.Client(), github.Config{
		APIBase: srv.URL, Owner: "org", Repo: "repo", Token: "ghp_xyz",
	}, "")
	if err != nil {
		t.Fatalf("FetchGocdnextFolder: %v", err)
	}
	if gotAuth != "Bearer ghp_xyz" {
		t.Fatalf("Authorization header = %q, want Bearer ghp_xyz", gotAuth)
	}
}

func TestFetchGocdnextFolder_FallsBackToDownloadURL(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/repos/org/repo/contents/.gocdnext", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]entry{
			{
				Name: "big.yaml", Type: "file", Size: 2_000_000,
				Encoding: "none", Content: "",
				DownloadURL: srv.URL + "/raw/big.yaml",
			},
		})
	})
	mux.HandleFunc("/raw/big.yaml", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = io.WriteString(w, "name: huge\n")
	})

	got, err := github.FetchGocdnextFolder(context.Background(), srv.Client(), github.Config{
		APIBase: srv.URL, Owner: "org", Repo: "repo",
	}, "")
	if err != nil {
		t.Fatalf("FetchGocdnextFolder: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Content, "huge") {
		t.Fatalf("got = %+v", got)
	}
	if hits != 2 {
		t.Fatalf("round trips = %d, want 2 (list + raw)", hits)
	}
}
