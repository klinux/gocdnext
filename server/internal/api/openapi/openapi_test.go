package openapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpecEmbedded(t *testing.T) {
	if len(Spec()) == 0 {
		t.Fatal("openapi spec embed is empty — check //go:embed directive")
	}
	out := string(Spec())

	want := []string{
		"openapi: 3.1.0",
		"title: gocdnext API",
		"/api/v1/me",
		"/api/v1/runs",
		"/api/v1/projects",
		"/metrics",
		"/healthz",
		"/readyz",
		"bearerAuth",
		"cookieAuth",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("spec missing %q", w)
		}
	}
}

func TestHandlerServesSpec(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("content-type = %q, want application/yaml", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc == "" {
		t.Errorf("cache-control header missing")
	}
}
