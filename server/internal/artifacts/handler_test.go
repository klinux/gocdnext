package artifacts

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func mustServer(t *testing.T, maxBody int64) (*httptest.Server, *FilesystemStore) {
	t.Helper()
	fs := mustFS(t)
	r := chi.NewRouter()
	NewHandler(fs, nil, maxBody).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	// Rewrite the publicBase so signed URLs hit the test server.
	fs.publicBase = srv.URL
	return srv, fs
}

func TestHandler_PutGet_RoundTrip(t *testing.T) {
	srv, fs := mustServer(t, 0)
	ctx := context.Background()

	putURL, err := fs.SignedPutURL(ctx, "run/1/job/a/blob", time.Minute)
	if err != nil {
		t.Fatalf("sign put: %v", err)
	}
	if !strings.HasPrefix(putURL.URL, srv.URL+"/artifacts/") {
		t.Fatalf("url: got %q", putURL.URL)
	}

	body := []byte("test-artifact-bytes")
	req, _ := http.NewRequest(http.MethodPut, putURL.URL, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put status = %d", resp.StatusCode)
	}

	getURL, _ := fs.SignedGetURL(ctx, "run/1/job/a/blob", time.Minute)
	resp2, err := http.Get(getURL.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	got, _ := io.ReadAll(resp2.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("get body mismatch: got %q", got)
	}
}

func TestHandler_BadToken(t *testing.T) {
	srv, _ := mustServer(t, 0)

	resp, err := http.Get(srv.URL + "/artifacts/not-a-token")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestHandler_WrongVerb(t *testing.T) {
	srv, fs := mustServer(t, 0)
	// A GET token used on a PUT URL must be rejected.
	getURL, _ := fs.SignedGetURL(context.Background(), "some/key", time.Minute)

	req, _ := http.NewRequest(http.MethodPut, getURL.URL, strings.NewReader("x"))
	req.ContentLength = 1
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	_ = srv
}

func TestHandler_Expired(t *testing.T) {
	srv, fs := mustServer(t, 0)
	// Mint a token already expired by signing with time in the past.
	tok := fs.signer.Sign("late/key", VerbPUT, time.Now().Add(-time.Minute))

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/artifacts/"+tok, strings.NewReader("x"))
	req.ContentLength = 1
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestHandler_GetMissing(t *testing.T) {
	srv, fs := mustServer(t, 0)
	getURL, _ := fs.SignedGetURL(context.Background(), "never/uploaded", time.Minute)

	resp, err := http.Get(getURL.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	_ = srv
}

func TestHandler_MaxBody(t *testing.T) {
	_, fs := mustServer(t, 5) // cap 5 bytes
	putURL, _ := fs.SignedPutURL(context.Background(), "big/one", time.Minute)

	body := []byte("this-is-way-more-than-five-bytes")
	req, _ := http.NewRequest(http.MethodPut, putURL.URL, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}
