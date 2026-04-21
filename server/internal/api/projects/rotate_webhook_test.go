package projects_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// mountRotate wires the rotate-webhook-secret route on a chi router so
// chi.URLParam resolves the {slug} during the test. httptest's default
// mux doesn't thread chi context, so skipping the router would make the
// handler see an empty slug and answer 400.
func mountRotate(t *testing.T) (http.Handler, func(string) *httptest.ResponseRecorder) {
	t.Helper()
	h, _ := newHandler(t)
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{slug}/scm-sources/rotate-webhook-secret", h.RotateWebhookSecret)

	// Seed the project + scm_source via Apply so the rotate handler
	// has a row to rotate against. Uses the same helper tests below
	// lean on elsewhere in this package.
	applyBody := map[string]any{
		"slug":  "rotating",
		"name":  "Rotating",
		"files": []map[string]string{},
		"scm_source": map[string]string{
			"provider":       "github",
			"url":            "https://github.com/org/rotating",
			"default_branch": "main",
		},
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(applyBody); err != nil {
		t.Fatalf("encode: %v", err)
	}
	applyReq := httptest.NewRequest(http.MethodPost, "/api/v1/projects/apply", &buf)
	applyReq.Header.Set("Content-Type", "application/json")
	applyRR := httptest.NewRecorder()
	h.Apply(applyRR, applyReq)
	if applyRR.Code != http.StatusOK {
		t.Fatalf("seed apply: status=%d body=%s", applyRR.Code, applyRR.Body.String())
	}

	do := func(slug string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/projects/"+slug+"/scm-sources/rotate-webhook-secret",
			nil,
		)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}
	return r, do
}

func TestRotateWebhookSecret_ReturnsPlaintextOnce(t *testing.T) {
	_, do := mountRotate(t)

	rr := do("rotating")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		SCMSourceID            string `json:"scm_source_id"`
		GeneratedWebhookSecret string `json:"generated_webhook_secret"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SCMSourceID == "" {
		t.Fatalf("scm_source_id empty")
	}
	// newWebhookSecret produces 64 hex chars (32 random bytes).
	if got := len(resp.GeneratedWebhookSecret); got != 64 {
		t.Fatalf("secret len = %d, want 64", got)
	}
}

func TestRotateWebhookSecret_ChangesEachCall(t *testing.T) {
	_, do := mountRotate(t)

	first := do("rotating")
	second := do("rotating")
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("codes = %d, %d", first.Code, second.Code)
	}
	var a, b struct {
		GeneratedWebhookSecret string `json:"generated_webhook_secret"`
	}
	_ = json.NewDecoder(first.Body).Decode(&a)
	_ = json.NewDecoder(second.Body).Decode(&b)
	if a.GeneratedWebhookSecret == "" || a.GeneratedWebhookSecret == b.GeneratedWebhookSecret {
		t.Fatalf("secrets didn't rotate: a=%q b=%q", a.GeneratedWebhookSecret, b.GeneratedWebhookSecret)
	}
}

func TestRotateWebhookSecret_UnknownSlug404(t *testing.T) {
	_, do := mountRotate(t)
	rr := do("does-not-exist")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRotateWebhookSecret_MethodNotAllowed(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/x/scm-sources/rotate-webhook-secret", nil)
	rr := httptest.NewRecorder()
	h.RotateWebhookSecret(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rr.Code)
	}
}
