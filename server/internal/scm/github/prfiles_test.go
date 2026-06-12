package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestFetchPRFiles(t *testing.T) {
	t.Run("single page with rename", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("Authorization = %q, want Bearer tok", got)
			}
			if r.URL.Path != "/repos/acme/shop/pulls/7/files" {
				t.Errorf("path = %s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"filename": "web/src/page.tsx"},
				{"filename": "internal/new_name.go", "previous_filename": "internal/old_name.go"},
			})
		}))
		defer srv.Close()

		files, complete, err := FetchPRFiles(context.Background(), srv.Client(),
			Config{APIBase: srv.URL, Owner: "acme", Repo: "shop", Token: "tok"}, 7)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !complete {
			t.Fatalf("complete = false, want true")
		}
		want := []string{"web/src/page.tsx", "internal/new_name.go", "internal/old_name.go"}
		if len(files) != len(want) {
			t.Fatalf("files = %v, want %v", files, want)
		}
	})

	t.Run("pagination walks pages until short page", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			n := 100
			if page == 3 {
				n = 5
			}
			out := make([]map[string]string, n)
			for i := range out {
				out[i] = map[string]string{"filename": fmt.Sprintf("p%d/f%d.go", page, i)}
			}
			_ = json.NewEncoder(w).Encode(out)
		}))
		defer srv.Close()

		files, complete, err := FetchPRFiles(context.Background(), srv.Client(),
			Config{APIBase: srv.URL, Owner: "a", Repo: "b"}, 1)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !complete || len(files) != 205 {
			t.Fatalf("files = %d complete = %v, want 205 true", len(files), complete)
		}
	})

	t.Run("pagination cap returns incomplete", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			out := make([]map[string]string, 100)
			for i := range out {
				out[i] = map[string]string{"filename": fmt.Sprintf("p%d/f%d.go", page, i)}
			}
			_ = json.NewEncoder(w).Encode(out)
		}))
		defer srv.Close()

		files, complete, err := FetchPRFiles(context.Background(), srv.Client(),
			Config{APIBase: srv.URL, Owner: "a", Repo: "b"}, 1)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if complete {
			t.Fatalf("complete = true past the page cap, want false (fail open)")
		}
		if len(files) != 3000 {
			t.Fatalf("files = %d, want 3000", len(files))
		}
	})

	t.Run("http error surfaces", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "rate limited", http.StatusForbidden)
		}))
		defer srv.Close()

		_, _, err := FetchPRFiles(context.Background(), srv.Client(),
			Config{APIBase: srv.URL, Owner: "a", Repo: "b"}, 1)
		if err == nil {
			t.Fatalf("expected error on 403")
		}
	})
}
