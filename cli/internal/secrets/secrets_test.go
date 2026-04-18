package secrets_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/cli/internal/secrets"
)

func TestSet_PostsJSONAndDecodesResponse(t *testing.T) {
	var gotBody secrets.SetRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/demo/secrets" {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(secrets.SetResponse{Name: "FOO", Created: true})
	}))
	defer srv.Close()

	got, err := secrets.Set(context.Background(), srv.Client(), srv.URL, "demo",
		secrets.SetRequest{Name: "FOO", Value: "bar"})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !got.Created || got.Name != "FOO" {
		t.Fatalf("got = %+v", got)
	}
	if gotBody.Name != "FOO" || gotBody.Value != "bar" {
		t.Fatalf("server-side body = %+v", gotBody)
	}
}

func TestSet_4xxIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "project not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := secrets.Set(context.Background(), srv.Client(), srv.URL, "nope",
		secrets.SetRequest{Name: "X", Value: "v"})
	if err == nil || !strings.Contains(err.Error(), "project not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestList_DecodesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(secrets.ListResponse{
			Secrets: []secrets.Secret{
				{Name: "A", CreatedAt: "2026-04-18T00:00:00Z", UpdatedAt: "2026-04-18T00:00:00Z"},
				{Name: "B", CreatedAt: "2026-04-18T00:00:00Z", UpdatedAt: "2026-04-18T00:00:00Z"},
			},
		})
	}))
	defer srv.Close()

	got, err := secrets.List(context.Background(), srv.Client(), srv.URL, "demo")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Name != "A" {
		t.Fatalf("got = %+v", got)
	}
}

func TestDelete_OnSuccessReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := secrets.Delete(context.Background(), srv.Client(), srv.URL, "demo", "FOO"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDelete_404Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret not found", http.StatusNotFound)
	}))
	defer srv.Close()

	err := secrets.Delete(context.Background(), srv.Client(), srv.URL, "demo", "MISSING")
	if err == nil || !strings.Contains(err.Error(), "secret not found") {
		t.Fatalf("err = %v", err)
	}
}
