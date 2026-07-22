package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func newClusterStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t)) // ResolveClusterForDispatch decrypts via authCipher
	return s, context.Background()
}

const sampleKubeconfig = "apiVersion: v1\nkind: Config\nclusters: []\n"

func TestClusters_Kubeconfig_RoundTripAndPreserve(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)

	c, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "prod-gke", Description: "prod", AuthType: store.ClusterAuthKubeconfig,
		Credential: sampleKubeconfig, CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if c.ID == uuid.Nil {
		t.Fatal("expected generated id")
	}

	// Cipher round-trip: dispatch resolver decrypts back to the exact
	// kubeconfig. (Get does NOT expose the credential — write-only.)
	kc, inCluster, masks, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "prod-gke")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if inCluster {
		t.Fatal("kubeconfig cluster must not be in_cluster")
	}
	if kc != sampleKubeconfig {
		t.Fatalf("kubeconfig round-trip mismatch: %q", kc)
	}
	// The whole blob is always masked; sampleKubeconfig has no sensitive
	// scalar, so that's the only entry.
	if len(masks) == 0 || masks[0] != sampleKubeconfig {
		t.Fatalf("masks = %v, want the kubeconfig blob first", masks)
	}

	// Update with the preserve sentinel + a metadata change keeps the
	// sealed credential.
	if err := s.UpdateCluster(ctx, cipher, c.ID, store.ClusterInput{
		Name: "prod-gke", Description: "prod (renamed desc)", AuthType: store.ClusterAuthKubeconfig,
		Credential: store.SecretPreserveSentinel,
	}); err != nil {
		t.Fatalf("update preserve: %v", err)
	}
	kc2, _, _, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "prod-gke")
	if err != nil {
		t.Fatalf("resolve after preserve: %v", err)
	}
	if kc2 != sampleKubeconfig {
		t.Fatalf("preserve-sentinel lost the credential: %q", kc2)
	}

	if err := s.DeleteCluster(ctx, c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetCluster(ctx, c.ID); !errors.Is(err, store.ErrClusterNotFound) {
		t.Fatalf("get after delete = %v, want ErrClusterNotFound", err)
	}
}

func TestClusters_Token_SynthesizesKubeconfig(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	_, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "tok", AuthType: store.ClusterAuthToken,
		APIServer: "https://k8s.example.com:6443", CACert: []byte("-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"),
		Credential: "sa-bearer-token-xyz",
	})
	if err != nil {
		t.Fatalf("insert token: %v", err)
	}
	kc, inCluster, masks, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "tok")
	if err != nil {
		t.Fatalf("resolve token: %v", err)
	}
	if inCluster {
		t.Fatal("token cluster must not be in_cluster")
	}
	// Synthesized kubeconfig must carry the token + server (and embed
	// the CA, not skip TLS).
	if !strings.Contains(kc, "sa-bearer-token-xyz") || !strings.Contains(kc, "https://k8s.example.com:6443") {
		t.Fatalf("synthesized kubeconfig missing token/server:\n%s", kc)
	}
	if !strings.Contains(kc, "certificate-authority-data") {
		t.Fatalf("expected CA embedded, got:\n%s", kc)
	}
	// HIGH 4: the raw bearer token must be masked on its own — the agent
	// redacts logs line-by-line, so the multiline kubeconfig blob alone
	// would not catch the token if a plugin echoed it standalone.
	if !containsMask(masks, "sa-bearer-token-xyz") {
		t.Fatalf("raw token absent from masks %v — it would leak in a line-by-line redacted log", masks)
	}
	if !containsMask(masks, kc) {
		t.Fatalf("whole kubeconfig absent from masks %v", masks)
	}
	// The token path repeats the bearer token (raw + embedded in the
	// synthesized config) — masks must be deduplicated.
	seen := map[string]int{}
	for _, m := range masks {
		seen[m]++
		if seen[m] > 1 {
			t.Fatalf("duplicate mask %q in %v", m, masks)
		}
	}
}

func containsMask(masks []string, want string) bool {
	for _, m := range masks {
		if m == want {
			return true
		}
	}
	return false
}

// TestClusters_Token_RequiresCA pins HIGH 3: token auth must never fall
// back to insecure-skip-tls-verify. A CA-less token cluster is rejected
// at insert (validateClusterInput) — synthesis never even runs.
func TestClusters_Token_RequiresCA(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	_, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "no-ca", AuthType: store.ClusterAuthToken,
		APIServer: "https://k8s.example.com:6443", Credential: "tok-without-ca",
	})
	if err == nil || !strings.Contains(err.Error(), "ca_cert") {
		t.Fatalf("CA-less token insert = %v, want a ca_cert error", err)
	}
}

// TestClusters_Token_RejectsBadAPIServer pins the api_server hardening:
// token auth needs a parseable https:// URL, no embedded userinfo, so a
// typo or http:// can't ship the bearer token in cleartext or create a
// cluster that only fails at deploy.
func TestClusters_Token_RejectsBadAPIServer(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	ca := []byte("-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n")
	tests := []struct {
		name      string
		apiServer string
	}{
		{"http scheme", "http://k8s.example.com:6443"},
		{"no scheme", "k8s.example.com:6443"},
		{"embedded userinfo", "https://user:pass@k8s.example.com:6443"},
		{"blank", "   "},
		{"garbage", "://::"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
				Name: "bad-srv", AuthType: store.ClusterAuthToken,
				APIServer: tt.apiServer, CACert: ca, Credential: "sa-token-value",
			})
			if err == nil || !strings.Contains(err.Error(), "api_server") {
				t.Fatalf("api_server %q = %v, want an api_server error", tt.apiServer, err)
			}
		})
	}
}

// TestClusters_RejectsExecKubeconfig pins the exec rejection: an
// exec-credential kubeconfig is unsupported (no auth binary shipped) and
// a masking blind spot, so it's refused at the cadastro, not at deploy.
func TestClusters_RejectsExecKubeconfig(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	const execKC = `apiVersion: v1
kind: Config
users:
- name: gke
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
`
	_, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "exec-kc", AuthType: store.ClusterAuthKubeconfig, Credential: execKC,
	})
	if err == nil || !strings.Contains(err.Error(), "exec-based") {
		t.Fatalf("exec kubeconfig insert = %v, want an exec-based rejection", err)
	}
}

// TestClusters_MasksNestedCredentials pins the recursive masker: a full
// kubeconfig can hide tokens deep under auth-provider.config.* — those
// nested scalars must still land in the mask set (the agent redacts
// line-by-line, so the multiline blob alone would not catch them).
func TestClusters_MasksNestedCredentials(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	const accessTok = "access-token-aaaaaaaaaaaa"
	const refreshTok = "refresh-token-rrrrrrrrrrrr"
	const idTok = "id-token-iiiiiiiiiiiiiiii"
	kc := `apiVersion: v1
kind: Config
users:
- name: oidc
  user:
    auth-provider:
      name: oidc
      config:
        access-token: ` + accessTok + `
        refresh-token: ` + refreshTok + `
        id-token: ` + idTok + `
`
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "oidc-kc", AuthType: store.ClusterAuthKubeconfig, Credential: kc,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, _, masks, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "oidc-kc")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, want := range []string{accessTok, refreshTok, idTok} {
		if !containsMask(masks, want) {
			t.Errorf("nested credential %q absent from masks %v — it would leak", want, masks)
		}
	}
}

func TestClusters_InCluster_NoCredential(t *testing.T) {
	s, ctx := newClusterStore(t)
	if _, err := s.InsertCluster(ctx, nil, store.ClusterInput{
		Name: "agent-local", AuthType: store.ClusterAuthInCluster,
	}); err != nil {
		t.Fatalf("insert in_cluster: %v", err)
	}
	kc, inCluster, masks, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "agent-local")
	if err != nil {
		t.Fatalf("resolve in_cluster: %v", err)
	}
	if !inCluster || kc != "" || len(masks) != 0 {
		t.Fatalf("in_cluster resolve = (%q, %v, %v), want (\"\", true, nil)", kc, inCluster, masks)
	}
	// in_cluster must reject a credential.
	if _, err := s.InsertCluster(ctx, nil, store.ClusterInput{
		Name: "bad", AuthType: store.ClusterAuthInCluster, Credential: "x",
	}); err == nil {
		t.Fatal("expected error: in_cluster takes no credential")
	}
}

func TestClusters_AllowedProjects_Scoping(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	projA, projB := uuid.New(), uuid.New()
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "scoped", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
		AllowedProjects: []string{projA.String()},
	}); err != nil {
		t.Fatalf("insert scoped: %v", err)
	}
	if _, _, _, err := s.ResolveClusterForDispatch(ctx, projA, "scoped"); err != nil {
		t.Fatalf("authorized project rejected: %v", err)
	}
	// The denial must carry the ErrClusterNotAuthorized sentinel — the deploy-target
	// registration maps it to 403 via errors.Is, so a regression to a string error
	// would silently break that mapping.
	if _, _, _, err := s.ResolveClusterForDispatch(ctx, projB, "scoped"); !errors.Is(err, store.ErrClusterNotAuthorized) {
		t.Fatalf("unauthorized project = %v, want ErrClusterNotAuthorized", err)
	}
}

func TestClusters_NameUnique_And_ResolveMissing(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "dup", AuthType: store.ClusterAuthInCluster,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "dup", AuthType: store.ClusterAuthInCluster,
	}); err == nil {
		t.Fatal("expected unique-name violation")
	}
	if _, _, _, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "ghost"); !errors.Is(err, store.ErrClusterNotFound) {
		t.Fatalf("resolve missing = %v, want ErrClusterNotFound", err)
	}
}

func TestResolveClusters_ApplyExistence(t *testing.T) {
	s, ctx := newClusterStore(t)
	if _, err := s.InsertCluster(ctx, nil, store.ClusterInput{
		Name: "prod", AuthType: store.ClusterAuthInCluster,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ok := []*domain.Pipeline{{Name: "p", Jobs: []domain.Job{{Name: "deploy", Cluster: "prod"}}}}
	if err := s.ResolveClusters(ctx, ok); err != nil {
		t.Fatalf("known cluster rejected: %v", err)
	}
	bad := []*domain.Pipeline{{Name: "p", Jobs: []domain.Job{{Name: "deploy", Cluster: "ghost"}}}}
	err := s.ResolveClusters(ctx, bad)
	if err == nil || !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown cluster err = %v, want one naming 'ghost'/'not registered'", err)
	}
}

func TestClusters_UpdateMissing_NotFound(t *testing.T) {
	s, ctx := newClusterStore(t)
	err := s.UpdateCluster(ctx, nil, uuid.New(), store.ClusterInput{
		Name: "ghost", AuthType: store.ClusterAuthInCluster,
	})
	if !errors.Is(err, store.ErrClusterNotFound) {
		t.Fatalf("update missing cluster = %v, want ErrClusterNotFound", err)
	}
}

// TestClusters_Update_RejectsEmptyCredential pins MED 1: a bare-empty
// credential on update is rejected (only the preserve sentinel keeps the
// existing one) so a direct API call can't silently seal an empty value
// and break the deploy.
func TestClusters_Update_RejectsEmptyCredential(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	c, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "kc", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	err = s.UpdateCluster(ctx, cipher, c.ID, store.ClusterInput{
		Name: "kc", AuthType: store.ClusterAuthKubeconfig, Credential: "",
	})
	if err == nil || !strings.Contains(err.Error(), "needs a credential") {
		t.Fatalf("empty-credential update = %v, want a 'needs a credential' error", err)
	}
	// The sealed credential survives the rejected update.
	kc, _, _, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "kc")
	if err != nil || kc != sampleKubeconfig {
		t.Fatalf("credential after rejected update = (%q, %v), want it preserved", kc, err)
	}
}

// TestClusters_Name_Immutable pins MED 4: the name is the dispatch-time
// identity of a `cluster:` reference, so an update can never change it
// (a rename would silently break every pipeline pointing at the cluster).
func TestClusters_Name_Immutable(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	c, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "stable", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Attempt a rename via update — the store ignores the name field.
	if err := s.UpdateCluster(ctx, cipher, c.ID, store.ClusterInput{
		Name: "renamed", AuthType: store.ClusterAuthKubeconfig, Credential: store.SecretPreserveSentinel,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetCluster(ctx, c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "stable" {
		t.Fatalf("name = %q, want it unchanged (immutable)", got.Name)
	}
	// The original name still resolves; the attempted new name does not.
	if _, _, _, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "stable"); err != nil {
		t.Fatalf("original name stopped resolving: %v", err)
	}
	if _, _, _, err := s.ResolveClusterForDispatch(ctx, uuid.New(), "renamed"); !errors.Is(err, store.ErrClusterNotFound) {
		t.Fatalf("renamed-to name resolved = %v, want ErrClusterNotFound", err)
	}
}

func TestClusters_DeleteGuard_Usage(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	// Seed a real pipeline, then inject a Cluster ref into its stored
	// definition so the jsonpath usage query has something to find.
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "cl-usage", Name: "cl-usage",
		Pipelines: []*domain.Pipeline{{
			Name:   "build",
			Stages: []string{"deploy"},
			Jobs:   []domain.Job{{Name: "ship", Stage: "deploy", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pid := res.Pipelines[0].PipelineID
	if _, err := pool.Exec(ctx,
		`UPDATE pipelines SET definition = $2 WHERE id = $1`,
		pid, []byte(`{"Name":"build","Stages":["deploy"],"Jobs":[{"Name":"ship","Stage":"deploy","Cluster":"prod"}]}`),
	); err != nil {
		t.Fatalf("inject cluster ref: %v", err)
	}

	u, err := s.CountClusterUsage(ctx, "prod")
	if err != nil {
		t.Fatalf("count usage: %v", err)
	}
	if u.Pipelines != 1 {
		t.Fatalf("usage.Pipelines = %d, want 1", u.Pipelines)
	}
	if other, _ := s.CountClusterUsage(ctx, "nope"); other.Pipelines != 0 {
		t.Fatalf("unreferenced usage.Pipelines = %d, want 0", other.Pipelines)
	}
}

// The declarative opt-in is a GOVERNANCE rule, not an authorization one: it decides
// whether a pipeline may register its own deploy target on a cluster. It must stay
// separate from AuthorizeClusterForProject, which the imperative UI/API path uses —
// folding it in there would start refusing legitimate admin edits.
func TestClusters_DeclarativeTargetAuthorization(t *testing.T) {
	s, ctx := newClusterStore(t)
	cipher := newAuthCipher(t)
	project := uuid.New()
	other := uuid.New()

	mk := func(name string, allowed []string, declarative bool) {
		t.Helper()
		if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
			Name: name, AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
			AllowedProjects: allowed, AllowDeclarativeTargets: declarative,
			CreatedBy: "admin@example.com",
		}); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	mk("open", nil, false)                                 // no governance expressed
	mk("governed", []string{project.String()}, false)      // curated, not opted in
	mk("governed-optin", []string{project.String()}, true) // curated AND opted in

	tests := []struct {
		name        string
		cluster     string
		as          uuid.UUID
		wantErr     error
		declarative bool // which helper to exercise
	}{
		// An open cluster grants nothing new: any project could already target it.
		{name: "open cluster allows declarative", cluster: "open", as: project, declarative: true},
		// Curated access means an admin decided who may use it — so an admin also
		// decides the target, unless they opt in explicitly.
		{name: "governed cluster denies declarative by default", cluster: "governed", as: project,
			declarative: true, wantErr: store.ErrClusterDeclarativeTargetsDisabled},
		{name: "governed + opt-in allows declarative", cluster: "governed-optin", as: project, declarative: true},
		// The opt-in never widens WHO: a project outside the allow-list is still out.
		{name: "opt-in does not bypass the allow-list", cluster: "governed-optin", as: other,
			declarative: true, wantErr: store.ErrClusterNotAuthorized},
		{name: "unknown cluster", cluster: "nope", as: project,
			declarative: true, wantErr: store.ErrClusterNotFound},

		// The REGRESSION the split exists to prevent: the imperative path must keep
		// working on a governed cluster that has NOT opted into declarative targets.
		{name: "imperative path unaffected by the flag", cluster: "governed", as: project},
		{name: "imperative path still enforces the allow-list", cluster: "governed", as: other,
			wantErr: store.ErrClusterNotAuthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.declarative {
				err = s.AuthorizeDeclarativeTargetClusterForProject(ctx, tt.as, tt.cluster)
			} else {
				err = s.AuthorizeClusterForProject(ctx, tt.as, tt.cluster)
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			// Every denial must be indistinguishable to the requester — telling
			// "not opted in" from "no such cluster" leaks that the cluster exists.
			if !store.IsClusterUnavailable(err) {
				t.Errorf("%v is not collapsed by IsClusterUnavailable — it would leak existence", err)
			}
		})
	}
}
