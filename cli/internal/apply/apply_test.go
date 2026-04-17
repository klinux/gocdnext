package apply_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/cli/internal/apply"
)

func writeFolder(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".gocdnext")
	if err := os.Mkdir(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func TestReadFolder_PicksYamlIgnoresOthers(t *testing.T) {
	root := writeFolder(t, map[string]string{
		"build.yaml":  "name: build\n",
		"deploy.yml":  "name: deploy\n",
		"README.md":   "nope",
		"notes.txt":   "nope",
	})

	got, err := apply.ReadFolder(root)
	if err != nil {
		t.Fatalf("ReadFolder: %v", err)
	}
	names := make([]string, len(got))
	for i, f := range got {
		names[i] = f.Name
	}
	sort.Strings(names)
	want := []string{"build.yaml", "deploy.yml"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestReadFolder_EmptyFolder(t *testing.T) {
	root := writeFolder(t, map[string]string{})
	_, err := apply.ReadFolder(root)
	if err == nil {
		t.Fatalf("want error on empty folder")
	}
}

func TestReadFolder_MissingFolder(t *testing.T) {
	_, err := apply.ReadFolder(t.TempDir())
	if err == nil {
		t.Fatalf("want error when .gocdnext missing")
	}
}

func TestPost_SendsRequestAndDecodesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/apply" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		var req apply.Request
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.Slug != "demo" || len(req.Files) != 1 {
			t.Errorf("req = %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apply.Response{
			ProjectID: "abc", ProjectCreated: true,
			Pipelines: []apply.PipelineStatus{{Name: "build", Created: true, MaterialsAdded: 1}},
		})
	}))
	defer srv.Close()

	got, err := apply.Post(context.Background(), srv.Client(), srv.URL, apply.Request{
		Slug: "demo", Files: []apply.File{{Name: "build.yaml", Content: "name: build"}},
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !got.ProjectCreated || got.ProjectID != "abc" {
		t.Fatalf("resp = %+v", got)
	}
}

func TestPost_ServerErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "slug is required", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := apply.Post(context.Background(), srv.Client(), srv.URL, apply.Request{
		Files: []apply.File{{Name: "x.yaml", Content: "n: 1"}},
	})
	if err == nil || !strings.Contains(err.Error(), "slug is required") {
		t.Fatalf("err = %v, want to contain server body", err)
	}
}
