package store_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestCheckClusterAccess_Denied(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)

	const tok = "rbac-token-xyz"
	var sawBody struct {
		Spec struct {
			ResourceAttributes struct {
				Group     string `json:"group"`
				Resource  string `json:"resource"`
				Verb      string `json:"verb"`
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"resourceAttributes"`
		} `json:"spec"`
	}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+tok {
			t.Errorf("auth = %q, want bearer token", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&sawBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":{"allowed":false,"reason":"RBAC: denied by role"}}`))
	}))
	defer ts.Close()

	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "rbac-denied", AuthType: store.ClusterAuthToken,
		APIServer: ts.URL, CACert: certPEM(ts), Credential: tok,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}

	err := s.CheckClusterAccess(ctx, uuid.New(), "rbac-denied", []store.ClusterAccessCheck{{
		Group: "argoproj.io", Resource: "applications", Verb: "patch", Namespace: "argocd", Name: "checkout",
	}})
	var denied *store.ClusterAccessDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("err = %v, want ClusterAccessDeniedError", err)
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatalf("token leaked in access-denied error: %v", err)
	}
	if sawBody.Spec.ResourceAttributes.Group != "argoproj.io" ||
		sawBody.Spec.ResourceAttributes.Resource != "applications" ||
		sawBody.Spec.ResourceAttributes.Verb != "patch" ||
		sawBody.Spec.ResourceAttributes.Namespace != "argocd" ||
		sawBody.Spec.ResourceAttributes.Name != "checkout" {
		t.Fatalf("resourceAttributes = %+v, want Application patch check", sawBody.Spec.ResourceAttributes)
	}
}

func TestCheckClusterAccess_InClusterSkipped(t *testing.T) {
	s, ctx := newClusterStore(t)
	if _, err := s.InsertCluster(ctx, nil, store.ClusterInput{Name: "local", AuthType: store.ClusterAuthInCluster}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	if err := s.CheckClusterAccess(ctx, uuid.New(), "local", []store.ClusterAccessCheck{{
		Group: "argoproj.io", Resource: "applications", Verb: "get", Namespace: "argocd", Name: "checkout",
	}}); err != nil {
		t.Fatalf("in_cluster access review should be skipped, got %v", err)
	}
}
