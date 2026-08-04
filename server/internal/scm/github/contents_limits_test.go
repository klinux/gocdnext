package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/scm/github"
)

const oneMiB = 1 << 20

func rawEntry(name, downloadURL string) entry {
	return entry{Name: name, Path: ".gocdnext/" + name, Type: "file", Encoding: "none", DownloadURL: downloadURL}
}

// A file over the 1 MiB per-file limit, served via download_url, is rejected —
// the read is bounded (io.LimitReader), never a whole io.ReadAll.
func TestFetchGocdnextFolder_RawFileOverLimitRejected(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/repos/org/repo/contents/.gocdnext", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]entry{rawEntry("big.yaml", srv.URL+"/raw/big.yaml")})
	})
	mux.HandleFunc("/raw/big.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("a", oneMiB+1))
	})
	_, err := github.FetchGocdnextFolder(context.Background(), srv.Client(),
		github.Config{APIBase: srv.URL, Owner: "org", Repo: "repo"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "per-file limit") {
		t.Fatalf("err = %v, want a per-file limit error", err)
	}
}

// Inline base64 content over the 1 MiB per-file limit is rejected — not silently
// re-fetched via download_url.
func TestFetchGocdnextFolder_InlineOverLimitRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]entry{inline("big.yaml", strings.Repeat("a", oneMiB+1))})
	}))
	defer srv.Close()
	_, err := github.FetchGocdnextFolder(context.Background(), srv.Client(),
		github.Config{APIBase: srv.URL, Owner: "org", Repo: "repo"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "per-file limit") {
		t.Fatalf("err = %v, want a per-file limit error", err)
	}
}

// Files summing over the 2 MiB total limit are rejected.
func TestFetchGocdnextFolder_TotalOverLimitRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		big := strings.Repeat("a", 800*1024) // ~0.8 MiB each; 3 → 2.4 MiB > 2 MiB
		_ = json.NewEncoder(w).Encode([]entry{inline("a.yaml", big), inline("b.yaml", big), inline("c.yaml", big)})
	}))
	defer srv.Close()
	_, err := github.FetchGocdnextFolder(context.Background(), srv.Client(),
		github.Config{APIBase: srv.URL, Owner: "org", Repo: "repo"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "total limit") {
		t.Fatalf("err = %v, want a total limit error", err)
	}
}

// More than 128 config files is rejected.
func TestFetchGocdnextFolder_TooManyFilesRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		es := make([]entry, 0, 129)
		for i := 0; i < 129; i++ {
			es = append(es, inline(fmt.Sprintf("f%d.yaml", i), "x"))
		}
		_ = json.NewEncoder(w).Encode(es)
	}))
	defer srv.Close()
	_, err := github.FetchGocdnextFolder(context.Background(), srv.Client(),
		github.Config{APIBase: srv.URL, Owner: "org", Repo: "repo"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v, want a too-many-files error", err)
	}
}
