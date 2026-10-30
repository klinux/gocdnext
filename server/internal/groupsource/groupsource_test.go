package groupsource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const testToken = "s3cr3t-platform-token"

func alice() store.ApproverIdentity {
	return store.ApproverIdentity{
		UserID: uuid.New(), Label: "Alice", Email: "alice@example.com",
		Provider: "oidc", Subject: "0f3c-11",
	}
}

// newSource starts a TLS test server running h and a source pointed at it.
func newSource(t *testing.T, h http.HandlerFunc) (*HTTP, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	src, err := New(Options{URL: srv.URL + "/groups", Token: testToken, Timeout: time.Second, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return src, srv
}

func groupsJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestApproverGroupNames_SendsStableSubjectAndBearer(t *testing.T) {
	var got *http.Request
	src, _ := newSource(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		groupsJSON(`{"groups":["sre","platform"]}`)(w, r)
	})

	groups, err := src.ApproverGroupNames(context.Background(), alice())
	if err != nil {
		t.Fatalf("ApproverGroupNames: %v", err)
	}
	if strings.Join(groups, ",") != "sre,platform" {
		t.Errorf("groups = %v, want [sre platform]", groups)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/groups" {
		t.Errorf("request = %s %s, want GET /groups", got.Method, got.URL.Path)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer "+testToken {
		t.Errorf("Authorization = %q", h)
	}
	q := got.URL.Query()
	if q.Get("provider") != "oidc" || q.Get("subject") != "0f3c-11" {
		t.Errorf("query = %v, want provider=oidc subject=0f3c-11", q)
	}
	// Only the stable subject identifies the user; display strings drift (ADR-0002).
	if q.Has("email") || q.Has("name") || strings.Contains(got.URL.RawQuery, "alice") {
		t.Errorf("query leaks display identity: %q", got.URL.RawQuery)
	}
}

// The subject is escaped, not spliced: a URN-style sub can't add parameters.
func TestApproverGroupNames_EscapesSubject(t *testing.T) {
	var q string
	src, _ := newSource(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query().Get("subject")
		groupsJSON(`{"groups":[]}`)(w, r)
	})
	who := alice()
	who.Subject = "urn:x:a&provider=evil b"
	if _, err := src.ApproverGroupNames(context.Background(), who); err != nil {
		t.Fatalf("ApproverGroupNames: %v", err)
	}
	if q != who.Subject {
		t.Errorf("subject = %q, want %q", q, who.Subject)
	}
}

func TestApproverGroupNames_Answers(t *testing.T) {
	tests := []struct {
		name    string
		h       http.HandlerFunc
		want    []string
		wantErr bool
	}{
		{"unknown user is no groups, not an outage", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}, nil, false},
		{"null groups", groupsJSON(`{"groups":null}`), nil, false},
		{"unknown fields tolerated", groupsJSON(`{"groups":["sre"],"next":"x"}`), []string{"sre"}, false},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, nil, true},
		{"unauthorized", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}, nil, true},
		// A redirect could carry the bearer token to another host: never followed.
		{"redirect not followed", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.invalid/groups", http.StatusFound)
		}, nil, true},
		{"malformed json", groupsJSON(`{"groups":[`), nil, true},
		{"wrong shape", groupsJSON(`{"groups":"sre"}`), nil, true},
		{"empty group name", groupsJSON(`{"groups":["sre",""]}`), nil, true},
		{"group name over bound", groupsJSON(`{"groups":["` + strings.Repeat("g", maxGroupNameLen+1) + `"]}`), nil, true},
		{"too many groups", groupsJSON(`{"groups":[` + strings.TrimSuffix(strings.Repeat(`"g",`, maxGroups+1), ",") + `]}`), nil, true},
		{"body over bound", groupsJSON(`{"groups":["` + strings.Repeat("g", maxBodyBytes) + `"]}`), nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, _ := newSource(t, tt.h)
			got, err := src.ApproverGroupNames(context.Background(), alice())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("groups = %v, want %v", got, tt.want)
			}
		})
	}
}

// Errors reach the server log via the 503 path: they may name the status, never the
// token, the response body, or the queried subject.
func TestApproverGroupNames_ErrorsDoNotLeak(t *testing.T) {
	src, _ := newSource(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal detail 10.0.0.7", http.StatusBadGateway)
	})
	_, err := src.ApproverGroupNames(context.Background(), alice())
	if err == nil {
		t.Fatal("want error")
	}
	for _, leak := range []string{testToken, "10.0.0.7", "0f3c-11"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks %q", err, leak)
		}
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q should name the status", err)
	}

	// Transport errors wrap a *url.Error whose text carries the full URL (subject included).
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	dead.Close()
	src2, err := New(Options{URL: dead.URL, Token: testToken, Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = src2.ApproverGroupNames(context.Background(), alice())
	if err == nil || strings.Contains(err.Error(), "0f3c-11") {
		t.Errorf("transport error = %v, want error without the subject", err)
	}
}

// No external identity to resolve: answer "no groups" without asking the platform.
func TestApproverGroupNames_SkipsIdentitiesWithoutSubject(t *testing.T) {
	var calls atomic.Int32
	src, _ := newSource(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		groupsJSON(`{"groups":["sre"]}`)(w, r)
	})
	cases := map[string]func(*store.ApproverIdentity){
		"empty subject":   func(w *store.ApproverIdentity) { w.Subject = "" },
		"empty provider":  func(w *store.ApproverIdentity) { w.Provider = "" },
		"local user":      func(w *store.ApproverIdentity) { w.Provider = store.ProviderLocal },
		"service account": func(w *store.ApproverIdentity) { w.Provider = store.ProviderServiceAccount },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			who := alice()
			mut(&who)
			got, err := src.ApproverGroupNames(context.Background(), who)
			if err != nil || got != nil {
				t.Errorf("= %v, %v; want nil, nil", got, err)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("platform called %d times, want 0", n)
	}
}

func TestApproverGroupNames_Timeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	src, err := New(Options{URL: srv.URL, Token: testToken, Timeout: 50 * time.Millisecond, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	if _, err := src.ApproverGroupNames(context.Background(), alice()); err == nil {
		t.Fatal("want timeout error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("call took %v, want bounded by the 50ms timeout", d)
	}
}

func TestApproverGroupNames_CircuitBreaker(t *testing.T) {
	var calls atomic.Int32
	var healthy atomic.Bool
	src, _ := newSource(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		groupsJSON(`{"groups":["sre"]}`)(w, r)
	})
	now := time.Unix(1_700_000_000, 0)
	src.breaker.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < breakerThreshold; i++ {
		if _, err := src.ApproverGroupNames(ctx, alice()); err == nil {
			t.Fatalf("call %d: want error", i)
		}
	}
	// Open: refused without a round trip, still an error (the store fails closed).
	_, err := src.ApproverGroupNames(ctx, alice())
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if n := calls.Load(); n != breakerThreshold {
		t.Fatalf("calls = %d, want %d (open circuit must not call)", n, breakerThreshold)
	}

	// After the cooldown one probe goes through; success closes the circuit.
	now = now.Add(breakerCooldown)
	healthy.Store(true)
	if got, err := src.ApproverGroupNames(ctx, alice()); err != nil || len(got) != 1 {
		t.Fatalf("probe = %v, %v; want [sre], nil", got, err)
	}
	if _, err := src.ApproverGroupNames(ctx, alice()); err != nil {
		t.Fatalf("closed circuit: %v", err)
	}
}

func TestApproverGroupNames_FailedProbeReopens(t *testing.T) {
	var calls atomic.Int32
	src, _ := newSource(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	now := time.Unix(1_700_000_000, 0)
	src.breaker.now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < breakerThreshold; i++ {
		_, _ = src.ApproverGroupNames(ctx, alice()) // tripping the breaker; errors expected
	}
	now = now.Add(breakerCooldown)
	if _, err := src.ApproverGroupNames(ctx, alice()); err == nil || errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("probe err = %v, want the upstream failure", err)
	}
	if _, err := src.ApproverGroupNames(ctx, alice()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("after failed probe err = %v, want ErrCircuitOpen", err)
	}
	if n := calls.Load(); n != breakerThreshold+1 {
		t.Errorf("calls = %d, want %d", n, breakerThreshold+1)
	}
}

// A voter who hangs up mid-call says nothing about the platform's health.
func TestApproverGroupNames_CallerCancelDoesNotTrip(t *testing.T) {
	src, _ := newSource(t, groupsJSON(`{"groups":["sre"]}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < breakerThreshold+1; i++ {
		if _, err := src.ApproverGroupNames(ctx, alice()); err == nil {
			t.Fatal("canceled ctx: want error")
		}
	}
	if _, err := src.ApproverGroupNames(context.Background(), alice()); err != nil {
		t.Fatalf("after caller cancels: %v, want the circuit still closed", err)
	}
}

func TestApproverGroupNames_RecordsOutcome(t *testing.T) {
	src, _ := newSource(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	before := lookupCount(t, "not_found")
	if _, err := src.ApproverGroupNames(context.Background(), alice()); err != nil {
		t.Fatalf("ApproverGroupNames: %v", err)
	}
	if got := lookupCount(t, "not_found"); got != before+1 {
		t.Errorf("not_found lookups = %d, want %d", got, before+1)
	}
}

// lookupCount reads the sample count of the lookup histogram for one outcome.
func lookupCount(t *testing.T, outcome string) uint64 {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "gocdnext_approver_group_lookup_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "outcome" && lp.GetValue() == outcome {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

func TestNew_Validates(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"https ok", Options{URL: "https://zen.example.com/api/groups", Token: testToken}, false},
		{"loopback http ok", Options{URL: "http://127.0.0.1:8080/groups", Token: testToken}, false},
		{"localhost http ok", Options{URL: "http://localhost:8080/groups", Token: testToken}, false},
		{"remote http refused", Options{URL: "http://zen.example.com/groups", Token: testToken}, true},
		{"empty url", Options{Token: testToken}, true},
		{"relative url", Options{URL: "/groups", Token: testToken}, true},
		{"url with userinfo", Options{URL: "https://u:p@zen.example.com/groups", Token: testToken}, true},
		{"url with query", Options{URL: "https://zen.example.com/groups?x=1", Token: testToken}, true},
		{"empty token", Options{URL: "https://zen.example.com/groups"}, true},
		{"negative timeout", Options{URL: "https://zen.example.com/groups", Token: testToken, Timeout: -time.Second}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), testToken) {
				t.Errorf("error %q leaks the token", err)
			}
		})
	}
}
