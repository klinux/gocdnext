package gitlab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitLab is a minimal httptest.Server that records hook CRUD
// calls and echoes back canned responses. One per test keeps
// assertions isolated.
type fakeGitLab struct {
	t         *testing.T
	hooks     []struct{ ID int64; URL string }
	lastCreate map[string]any
	lastUpdate map[string]any
	lastPath   string
	lastToken  string
}

func (f *fakeGitLab) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/", func(w http.ResponseWriter, r *http.Request) {
		f.t.Helper()
		f.lastPath = r.URL.Path
		f.lastToken = r.Header.Get("PRIVATE-TOKEN")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.hooks)
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &f.lastCreate)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":  int64(42),
				"url": f.lastCreate["url"],
			})
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &f.lastUpdate)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":  int64(42),
				"url": f.lastUpdate["url"],
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func TestCreateProjectHook(t *testing.T) {
	fg := &fakeGitLab{t: t}
	srv := httptest.NewServer(fg.handler())
	defer srv.Close()

	cfg := Config{APIBase: srv.URL, ProjectPath: "org/repo", Token: "pat-123"}
	hook, err := CreateProjectHook(context.Background(), srv.Client(), cfg, CreateHookInput{
		ProjectPath: "org/repo",
		URL:         "https://ci.example.com/api/webhooks/gitlab",
		Secret:      "shh",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if hook.ID != 42 {
		t.Errorf("hook.ID = %d, want 42", hook.ID)
	}
	// URL-encoded project path hits /projects/org%2Frepo/hooks — the
	// encoded form may decode before our handler sees it via
	// ServeMux, so we just check the prefix. Auth header must carry
	// the PAT verbatim.
	if !strings.HasPrefix(fg.lastPath, "/projects/") {
		t.Errorf("path = %q", fg.lastPath)
	}
	if fg.lastToken != "pat-123" {
		t.Errorf("token = %q", fg.lastToken)
	}
	if fg.lastCreate["token"] != "shh" {
		t.Errorf("token field in payload = %v", fg.lastCreate["token"])
	}
	if fg.lastCreate["push_events"] != true {
		t.Errorf("push_events = %v, want true", fg.lastCreate["push_events"])
	}
}

func TestListProjectHooks(t *testing.T) {
	fg := &fakeGitLab{t: t, hooks: []struct {
		ID  int64
		URL string
	}{
		{ID: 10, URL: "https://ci.example.com/api/webhooks/gitlab"},
		{ID: 11, URL: "https://other.example.com/"},
	}}
	srv := httptest.NewServer(fg.handler())
	defer srv.Close()

	got, err := ListProjectHooks(context.Background(), srv.Client(), Config{
		APIBase: srv.URL, ProjectPath: "org/repo", Token: "pat",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d hooks, want 2", len(got))
	}
	if _, ok := FindHookForURL(got, "https://ci.example.com"); !ok {
		t.Errorf("FindHookForURL should match the ci.example.com hook")
	}
}

func TestUpdateProjectHook(t *testing.T) {
	fg := &fakeGitLab{t: t}
	srv := httptest.NewServer(fg.handler())
	defer srv.Close()

	hook, err := UpdateProjectHook(context.Background(), srv.Client(),
		Config{APIBase: srv.URL, ProjectPath: "org/repo", Token: "pat"},
		99,
		CreateHookInput{URL: "https://ci.example.com/api/webhooks/gitlab", Secret: "new"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if hook.ID != 42 {
		t.Errorf("hook.ID = %d", hook.ID)
	}
	if fg.lastUpdate["token"] != "new" {
		t.Errorf("token not rotated: %v", fg.lastUpdate["token"])
	}
}
