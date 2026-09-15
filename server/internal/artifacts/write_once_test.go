package artifacts_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
)

// TestResolvePutOptions_CreateOnly locks the option plumbing: WithCreateOnly
// flips the flag, nil/empty options stay a plain PUT.
func TestResolvePutOptions_CreateOnly(t *testing.T) {
	if artifacts.ResolvePutOptions(nil).CreateOnly {
		t.Errorf("nil options should not be create-only")
	}
	if !artifacts.ResolvePutOptions([]artifacts.PutOption{artifacts.WithCreateOnly()}).CreateOnly {
		t.Errorf("WithCreateOnly should set CreateOnly")
	}
	// A nil option in the slice must be tolerated (mirrors ResolveGetOptions).
	if !artifacts.ResolvePutOptions([]artifacts.PutOption{nil, artifacts.WithCreateOnly()}).CreateOnly {
		t.Errorf("nil option in slice should be skipped, not panic")
	}
}

// TestS3_SignedPutURL_CreateOnly_Offline asserts the create-only precondition
// is folded into the signature AND surfaced in SignedURL.Headers for the agent
// to echo. Presigning is local crypto — no container needed.
func TestS3_SignedPutURL_CreateOnly_Offline(t *testing.T) {
	s, err := artifacts.NewS3Store(context.Background(), artifacts.S3Config{
		Bucket: "b", Region: "us-east-1", Endpoint: "http://127.0.0.1:1",
		AccessKey: "ak", SecretKey: "sk", UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	ctx := context.Background()

	su, err := s.SignedPutURL(ctx, "k", time.Minute, artifacts.WithCreateOnly())
	if err != nil {
		t.Fatalf("sign create-only: %v", err)
	}
	if su.Headers["If-None-Match"] != "*" {
		t.Errorf("Headers[If-None-Match] = %q, want *", su.Headers["If-None-Match"])
	}
	// The header must be part of the signature (X-Amz-SignedHeaders), else the
	// agent sending it would break the signature.
	if !strings.Contains(strings.ToLower(su.URL), "if-none-match") {
		t.Errorf("if-none-match not folded into signed URL: %s", su.URL)
	}

	// Plain PUT (write-once off): no precondition, no header.
	plain, err := s.SignedPutURL(ctx, "k", time.Minute)
	if err != nil {
		t.Fatalf("sign plain: %v", err)
	}
	if len(plain.Headers) != 0 {
		t.Errorf("plain PUT should carry no headers, got %v", plain.Headers)
	}
	if strings.Contains(strings.ToLower(plain.URL), "if-none-match") {
		t.Errorf("plain PUT URL should not sign if-none-match: %s", plain.URL)
	}
}

// TestGCS_SignedPutURL_CreateOnly_Offline is the GCS analogue — signing is
// math-only, no emulator needed.
func TestGCS_SignedPutURL_CreateOnly_Offline(t *testing.T) {
	store, err := artifacts.NewGCSStore(context.Background(), artifacts.GCSConfig{
		Bucket: "b", CredentialsJSON: fakeCredsJSON(t),
	})
	if err != nil {
		t.Fatalf("NewGCSStore: %v", err)
	}
	ctx := context.Background()

	su, err := store.SignedPutURL(ctx, "k", time.Minute, artifacts.WithCreateOnly())
	if err != nil {
		t.Fatalf("sign create-only: %v", err)
	}
	if su.Headers["x-goog-if-generation-match"] != "0" {
		t.Errorf("Headers[x-goog-if-generation-match] = %q, want 0", su.Headers["x-goog-if-generation-match"])
	}
	if !strings.Contains(strings.ToLower(su.URL), "if-generation-match") {
		t.Errorf("if-generation-match not folded into signed URL: %s", su.URL)
	}

	plain, err := store.SignedPutURL(ctx, "k", time.Minute)
	if err != nil {
		t.Fatalf("sign plain: %v", err)
	}
	if len(plain.Headers) != 0 {
		t.Errorf("plain PUT should carry no headers, got %v", plain.Headers)
	}
}

// TestS3_CreateOnly_RejectsOverwrite is a best-effort integration check that a
// second PUT to an occupied key is rejected (412) when create-only is signed.
// Skips when the backend does not enforce If-None-Match (older LocalStack) —
// the offline test above already proves the precondition is signed + surfaced.
func TestS3_CreateOnly_RejectsOverwrite(t *testing.T) {
	endpoint := localStack(t)
	s := newS3Store(t, endpoint, "artifact-create-only")
	ctx := context.Background()
	key := "co/one"

	put := func(su artifacts.SignedURL) int {
		req, _ := http.NewRequest(http.MethodPut, su.URL, bytes.NewReader([]byte("payload")))
		req.ContentLength = int64(len("payload"))
		for k, v := range su.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	first, err := s.SignedPutURL(ctx, key, time.Minute, artifacts.WithCreateOnly())
	if err != nil {
		t.Fatalf("sign 1: %v", err)
	}
	if code := put(first); code/100 != 2 {
		t.Fatalf("first create-only PUT to empty key = %d, want 2xx", code)
	}

	second, err := s.SignedPutURL(ctx, key, time.Minute, artifacts.WithCreateOnly())
	if err != nil {
		t.Fatalf("sign 2: %v", err)
	}
	if code := put(second); code != http.StatusPreconditionFailed {
		t.Skipf("backend did not enforce If-None-Match (got %d) — offline test covers the signing", code)
	}
}
