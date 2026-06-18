package store_test

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func certPEM(ts *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
}

func mustInsertToken(t *testing.T, s *store.Store, ctx context.Context, cipher *crypto.Cipher, name, server string, ca []byte, tok string) uuid.UUID {
	t.Helper()
	c, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: name, AuthType: store.ClusterAuthToken,
		APIServer: server, CACert: ca, Credential: tok,
	})
	if err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
	return c.ID
}

// TestProbeCluster_Token spins a TLS server that stands in for a k8s API
// and checks the three meaningful outcomes: right token + pinned CA → ok
// (with the version), wrong token → unauthorized, and the token never
// appears in the result message.
func TestProbeCluster_Token(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)

	const goodTok = "sa-bearer-token-xyz"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+goodTok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gitVersion":"v1.30.2"}`))
	}))
	defer ts.Close()
	ca := certPEM(ts)

	okID := mustInsertToken(t, s, ctx, cipher, "tok-ok", ts.URL, ca, goodTok)
	res, err := s.ProbeCluster(ctx, okID)
	if err != nil {
		t.Fatalf("probe ok: %v", err)
	}
	if res.Status != store.ClusterProbeOK || !strings.Contains(res.Message, "v1.30.2") {
		t.Fatalf("ok probe = %+v, want ok + version", res)
	}
	if strings.Contains(res.Message, goodTok) {
		t.Fatalf("token leaked into probe message: %q", res.Message)
	}

	badID := mustInsertToken(t, s, ctx, cipher, "tok-bad", ts.URL, ca, "wrong-token")
	res, err = s.ProbeCluster(ctx, badID)
	if err != nil {
		t.Fatalf("probe bad: %v", err)
	}
	if res.Status != store.ClusterProbeUnauthorized {
		t.Fatalf("bad-token probe = %+v, want unauthorized", res)
	}
}

// TestProbeCluster_InCluster_Skipped: the control plane can't reach the
// agent pod's ServiceAccount, so in_cluster is reported as skipped.
func TestProbeCluster_InCluster_Skipped(t *testing.T) {
	s, ctx := newClusterStore(t)
	c, err := s.InsertCluster(ctx, nil, store.ClusterInput{Name: "local", AuthType: store.ClusterAuthInCluster})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := s.ProbeCluster(ctx, c.ID)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Status != store.ClusterProbeSkipped {
		t.Fatalf("in_cluster probe = %+v, want skipped", res)
	}
}

// TestProbeCluster_Unreachable: a valid CA but a dead endpoint (the
// server is closed before the probe) → unreachable, not a 500.
func TestProbeCluster_Unreachable(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ca := certPEM(ts)
	url := ts.URL
	ts.Close() // now nothing listens

	id := mustInsertToken(t, s, ctx, cipher, "dead", url, ca, "tok")
	res, err := s.ProbeCluster(ctx, id)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Status != store.ClusterProbeUnreachable {
		t.Fatalf("dead-endpoint probe = %+v, want unreachable", res)
	}
}

// TestProbeCluster_NotFound: a missing id surfaces ErrClusterNotFound.
func TestProbeCluster_NotFound(t *testing.T) {
	s, ctx := newClusterStore(t)
	if _, err := s.ProbeCluster(ctx, uuid.New()); err == nil {
		t.Fatal("expected an error for a missing cluster")
	}
}

func kcWith(server, caB64, token string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: ctx
clusters:
- name: c1
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: u1
  user:
    token: %s
contexts:
- name: ctx
  context:
    cluster: c1
    user: u1
`, server, caB64, token)
}

// TestProbeCluster_Kubeconfig_OK exercises the kubeconfig parse + probe
// happy path against the stand-in TLS server.
func TestProbeCluster_Kubeconfig_OK(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	const tok = "kc-bearer"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" && r.Header.Get("Authorization") == "Bearer "+tok {
			_, _ = w.Write([]byte(`{"gitVersion":"v1.28.0"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	caB64 := base64.StdEncoding.EncodeToString(certPEM(ts))

	c, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "kc-ok", AuthType: store.ClusterAuthKubeconfig,
		Credential: kcWith(ts.URL, caB64, tok),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := s.ProbeCluster(ctx, c.ID)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Status != store.ClusterProbeOK || !strings.Contains(res.Message, "v1.28.0") {
		t.Fatalf("kubeconfig probe = %+v, want ok + version", res)
	}
}

// TestProbeCluster_Kubeconfig_Rejections: malformed/unsupported
// kubeconfigs fail with a clear message instead of producing a
// misleading unauthorized/unreachable (or a false ok).
func TestProbeCluster_Kubeconfig_Rejections(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	tests := []struct {
		kc   string
		want string
	}{
		{kcWith("http://k8s.example.com:6443", "", "tok"), "https"},
		{kcWith("https://user@k8s.example.com:6443", "", "tok"), "userinfo"},
		{`apiVersion: v1
kind: Config
clusters:
- {name: c1, cluster: {server: https://k.example.com}}
users:
- {name: u1, user: {token: t}}`, "current-context"},
		{`apiVersion: v1
kind: Config
current-context: ctx
clusters:
- {name: c1, cluster: {server: https://k.example.com, certificate-authority: /etc/ca.crt}}
users:
- {name: u1, user: {token: t}}
contexts:
- {name: ctx, context: {cluster: c1, user: u1}}`, "file-path CA"},
		{`apiVersion: v1
kind: Config
current-context: ctx
clusters:
- {name: c1, cluster: {server: https://k.example.com}}
users:
- {name: u1, user: {client-key: /etc/k.key, client-certificate: /etc/c.crt}}
contexts:
- {name: ctx, context: {cluster: c1, user: u1}}`, "file-path credentials"},
		{`apiVersion: v1
kind: Config
current-context: ctx
clusters:
- {name: c1, cluster: {server: https://k.example.com}}
users:
- {name: u1, user: {}}
contexts:
- {name: ctx, context: {cluster: c1, user: u1}}`, "no supported credential"},
	}
	for i, tt := range tests {
		c, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
			Name: fmt.Sprintf("kc-rej-%d", i), AuthType: store.ClusterAuthKubeconfig, Credential: tt.kc,
		})
		if err != nil {
			t.Fatalf("case %d insert: %v", i, err)
		}
		res, err := s.ProbeCluster(ctx, c.ID)
		if err != nil {
			t.Fatalf("case %d probe: %v", i, err)
		}
		if res.Status != store.ClusterProbeError || !strings.Contains(res.Message, tt.want) {
			t.Fatalf("case %d probe = %+v, want error containing %q", i, res, tt.want)
		}
	}
}

// TestProbeCluster_Redirect_Refused: a 3xx from /version is reported as
// an error — the probe must NOT follow it with the credential attached.
func TestProbeCluster_Redirect_Refused(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.com/version", http.StatusFound)
	}))
	defer ts.Close()
	id := mustInsertToken(t, s, ctx, cipher, "redir", ts.URL, certPEM(ts), "tok")
	res, err := s.ProbeCluster(ctx, id)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Status != store.ClusterProbeError || !strings.Contains(res.Message, "redirect") {
		t.Fatalf("redirect probe = %+v, want error mentioning redirect", res)
	}
}
