package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"gopkg.in/yaml.v3"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrClusterNotFound is returned when a cluster id/name doesn't resolve.
var ErrClusterNotFound = errors.New("store: cluster not found")

// ErrClusterInUse is returned when a cluster can't be deleted because a row still
// references it through a RESTRICT foreign key (e.g. a deploy_target) — the
// race-proof backstop for the handler's usage pre-check.
var ErrClusterInUse = errors.New("store: cluster in use")

// ErrClusterNotAuthorized is returned when a project isn't in a cluster's
// allowed_projects — a typed error so callers map it to 403 without string-matching.
var ErrClusterNotAuthorized = errors.New("store: project not authorized for cluster")

// pgForeignKeyViolation is Postgres SQLSTATE 23503.
const pgForeignKeyViolation = "23503"

// Cluster auth types. kubeconfig/token carry a sealed credential;
// in_cluster carries none (the job pod's mounted SA is used).
const (
	ClusterAuthKubeconfig = "kubeconfig"
	ClusterAuthToken      = "token"
	ClusterAuthInCluster  = "in_cluster"
)

// clusterNameRE bounds cluster names to a YAML-safe, DNS-ish shape so
// a `cluster:` reference can't smuggle anything into the jsonpath
// usage query or a log line.
var clusterNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Cluster is the store-level view returned to the admin API. The
// credential is NEVER included — the registry surface is write-only,
// mirroring global secrets.
type Cluster struct {
	ID              uuid.UUID
	Name            string
	Description     string
	AuthType        string
	APIServer       string
	CACert          []byte
	AllowedProjects []string // project ids as text; empty = any project
	CreatedBy       string
	CreatedAt       *time.Time
	UpdatedAt       *time.Time
}

// ClusterInput is the create/update payload.
type ClusterInput struct {
	Name        string
	Description string
	AuthType    string
	APIServer   string
	CACert      []byte
	// Credential is the kubeconfig (auth_type=kubeconfig) or the
	// bearer token (auth_type=token). Empty for in_cluster. On Update,
	// SecretPreserveSentinel keeps the existing sealed credential so an
	// edit that touches only metadata doesn't force a re-entry.
	Credential      string
	AllowedProjects []string
	CreatedBy       string
}

// ClusterUsage counts the live dependents a delete-guard cares about:
// pipelines whose definition still names the cluster, and queued/
// running runs against any such pipeline. Either > 0 blocks a delete.
type ClusterUsage struct {
	Pipelines  int
	ActiveRuns int
}

func validateClusterInput(in ClusterInput, isUpdate bool) error {
	if !clusterNameRE.MatchString(in.Name) {
		return fmt.Errorf("store: cluster name %q invalid (want %s)", in.Name, clusterNameRE.String())
	}
	switch in.AuthType {
	case ClusterAuthKubeconfig, ClusterAuthToken:
		// Credential required on insert AND update. On update the
		// preserve sentinel (a non-empty marker) stands in for "keep
		// the existing one"; a bare-empty credential is rejected on
		// both paths so a direct API update can't seal an empty value
		// and silently break the deploy / synthesize a token-less
		// kubeconfig.
		if in.Credential == "" {
			return fmt.Errorf("store: cluster %q: auth_type %q needs a credential", in.Name, in.AuthType)
		}
		if !isUpdate && in.Credential == SecretPreserveSentinel {
			return fmt.Errorf("store: cluster %q: the preserve sentinel is only valid on update", in.Name)
		}
		if in.AuthType == ClusterAuthToken {
			// token auth must verify TLS against a CA — never a silent
			// insecure-skip-tls-verify fallback. The CA is a public cert,
			// so it's required outright (re-sent on every update; the API
			// surfaces it). For a self-signed/insecure dev cluster, use a
			// full kubeconfig with the flag set explicitly instead.
			if len(in.CACert) == 0 {
				return fmt.Errorf("store: cluster %q: token auth requires a ca_cert (no insecure TLS fallback)", in.Name)
			}
			// The api_server is interpolated into the synthesized
			// kubeconfig and the bearer token rides this connection — so
			// it must be a parseable https:// URL with no embedded
			// userinfo. http:// would put the token on the wire in
			// cleartext; an unparseable value would only blow up at
			// deploy time, far from the typo.
			if err := validateAPIServerURL(in.APIServer); err != nil {
				return fmt.Errorf("store: cluster %q: %w", in.Name, err)
			}
		}
		// A full kubeconfig with an `exec:` user runs an external
		// credential binary (gke-gcloud-auth-plugin, aws-iam-authenticator,
		// …) that gocdnext's plugin images don't ship — it's unsupported
		// (see docs) and its secrets can hide in argv/env where the log
		// masker can't reach them. Reject it at the cadastro instead of
		// failing opaquely at deploy. Only checkable when the kubeconfig
		// is actually present (not the preserve sentinel).
		if in.AuthType == ClusterAuthKubeconfig &&
			in.Credential != "" && in.Credential != SecretPreserveSentinel {
			if err := rejectUnsupportedKubeconfig(in.Credential); err != nil {
				return fmt.Errorf("store: cluster %q: %w", in.Name, err)
			}
		}
	case ClusterAuthInCluster:
		if in.Credential != "" && in.Credential != SecretPreserveSentinel {
			return fmt.Errorf("store: cluster %q: in_cluster takes no credential", in.Name)
		}
	default:
		return fmt.Errorf("store: cluster %q: unknown auth_type %q", in.Name, in.AuthType)
	}
	return nil
}

// ListClusters returns every cluster (no credential), sorted by name.
func (s *Store) ListClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := s.q.ListClusters(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list clusters: %w", err)
	}
	out := make([]Cluster, 0, len(rows))
	for _, r := range rows {
		out = append(out, clusterFromRow(r.ID, r.Name, r.Description, r.AuthType,
			r.ApiServer, r.CaCert, r.AllowedProjects, r.CreatedBy, r.CreatedAt, r.UpdatedAt))
	}
	return out, nil
}

// GetCluster returns one cluster by id (no credential).
func (s *Store) GetCluster(ctx context.Context, id uuid.UUID) (Cluster, error) {
	r, err := s.q.GetCluster(ctx, pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Cluster{}, ErrClusterNotFound
	}
	if err != nil {
		return Cluster{}, fmt.Errorf("store: get cluster %s: %w", id, err)
	}
	return clusterFromRow(r.ID, r.Name, r.Description, r.AuthType, r.ApiServer,
		r.CaCert, r.AllowedProjects, r.CreatedBy, r.CreatedAt, r.UpdatedAt), nil
}

// InsertCluster seals the credential (kubeconfig/token) with the cipher
// and persists. Plaintext never reaches the DB. in_cluster stores nil.
func (s *Store) InsertCluster(ctx context.Context, cipher *crypto.Cipher, in ClusterInput) (Cluster, error) {
	in.APIServer = strings.TrimSpace(in.APIServer)
	if err := validateClusterInput(in, false); err != nil {
		return Cluster{}, err
	}
	credEnc, err := sealClusterCredential(cipher, in.AuthType, in.Credential)
	if err != nil {
		return Cluster{}, err
	}
	r, err := s.q.InsertCluster(ctx, db.InsertClusterParams{
		Name:            in.Name,
		Description:     in.Description,
		AuthType:        in.AuthType,
		ApiServer:       in.APIServer,
		CaCert:          in.CACert,
		CredentialEnc:   credEnc,
		AllowedProjects: normalizeProjectIDs(in.AllowedProjects),
		CreatedBy:       in.CreatedBy,
	})
	if err != nil {
		return Cluster{}, fmt.Errorf("store: insert cluster %q: %w", in.Name, err)
	}
	return clusterFromRow(r.ID, r.Name, r.Description, r.AuthType, r.ApiServer,
		r.CaCert, r.AllowedProjects, r.CreatedBy, r.CreatedAt, r.UpdatedAt), nil
}

// UpdateCluster rewrites a row in place. A Credential of
// SecretPreserveSentinel re-seals the existing ciphertext so a
// metadata-only edit doesn't force re-entering the kubeconfig.
func (s *Store) UpdateCluster(ctx context.Context, cipher *crypto.Cipher, id uuid.UUID, in ClusterInput) error {
	in.APIServer = strings.TrimSpace(in.APIServer)
	if err := validateClusterInput(in, true); err != nil {
		return err
	}
	var credEnc []byte
	if in.AuthType != ClusterAuthInCluster && in.Credential == SecretPreserveSentinel {
		existing, err := s.q.GetClusterCredentialEnc(ctx, pgUUID(id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrClusterNotFound
		}
		if err != nil {
			return fmt.Errorf("store: update cluster %s: lookup credential: %w", id, err)
		}
		credEnc = existing
	} else {
		var err error
		credEnc, err = sealClusterCredential(cipher, in.AuthType, in.Credential)
		if err != nil {
			return err
		}
	}
	rows, err := s.q.UpdateCluster(ctx, db.UpdateClusterParams{
		ID:              pgUUID(id),
		Description:     in.Description,
		AuthType:        in.AuthType,
		ApiServer:       in.APIServer,
		CaCert:          in.CACert,
		CredentialEnc:   credEnc,
		AllowedProjects: normalizeProjectIDs(in.AllowedProjects),
	})
	if err != nil {
		return fmt.Errorf("store: update cluster %s: %w", id, err)
	}
	if rows == 0 {
		// A missing id affects zero rows — surface it consistently
		// (the preserve-sentinel path 404s via its lookup; this covers
		// the non-preserve / in_cluster paths too).
		return ErrClusterNotFound
	}
	return nil
}

// DeleteCluster removes the row. Caller MUST have checked usage first.
func (s *Store) DeleteCluster(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM clusters WHERE id = $1`, pgUUID(id))
	if err != nil {
		// A referencing row (e.g. a deploy_target) created between the caller's
		// usage pre-check and this delete trips the RESTRICT FK; surface it as
		// "in use" (→ 409), not a generic 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
			return ErrClusterInUse
		}
		return fmt.Errorf("store: delete cluster %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClusterNotFound
	}
	return nil
}

// CountClusterUsage returns the live dependents of a cluster name in
// one round-trip — pipelines whose definition names it (jsonpath over
// Jobs) and queued/running runs against those pipelines.
func (s *Store) CountClusterUsage(ctx context.Context, name string) (ClusterUsage, error) {
	var u ClusterUsage
	err := s.pool.QueryRow(ctx, `
        WITH refs AS (
            SELECT id FROM pipelines
            WHERE jsonb_path_exists(
                definition,
                '$.Jobs[*] ? (@.Cluster == $name)',
                jsonb_build_object('name', $1::text)
            )
        )
        SELECT
            (SELECT COUNT(*) FROM refs)::INT AS pipelines,
            (SELECT COUNT(*) FROM runs r
                WHERE r.status IN ('queued', 'running')
                  AND r.pipeline_id IN (SELECT id FROM refs))::INT AS active_runs
    `, name).Scan(&u.Pipelines, &u.ActiveRuns)
	if err != nil {
		return ClusterUsage{}, fmt.Errorf("store: count cluster usage %q: %w", name, err)
	}
	return u, nil
}

// ResolveClusterForDispatch is the scheduler's dispatch-time resolver:
// it authorizes the project against allowed_projects, then returns a
// ready-to-use kubeconfig (decrypted, or synthesized from a token) for
// injection as PLUGIN_KUBECONFIG, plus the set of log masks that must
// be redacted. in_cluster returns ("", true, nil, nil) — no kubeconfig;
// the job pod's mounted SA is used. Uses the store's authCipher
// (runtime path), not a passed cipher.
//
// masks intentionally carries more than the whole kubeconfig blob: the
// agent redacts logs LINE-BY-LINE, so a multiline kubeconfig as a whole
// would never match a log line. The sensitive scalars inside it (bearer
// token, client key, password) — and, for token auth, the raw token —
// are single-line values that DO survive line-based redaction, so they
// are returned individually.
func (s *Store) ResolveClusterForDispatch(ctx context.Context, projectID uuid.UUID, name string) (kubeconfig string, inCluster bool, masks []string, err error) {
	r, err := s.q.GetClusterForDispatch(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil, ErrClusterNotFound
	}
	if err != nil {
		return "", false, nil, fmt.Errorf("store: resolve cluster %q: %w", name, err)
	}
	if !projectAllowed(r.AllowedProjects, projectID) {
		return "", false, nil, fmt.Errorf("%w %q", ErrClusterNotAuthorized, name)
	}
	switch r.AuthType {
	case ClusterAuthInCluster:
		return "", true, nil, nil
	case ClusterAuthKubeconfig:
		dec, err := s.decryptCredential(r.CredentialEnc, name)
		if err != nil {
			return "", false, nil, err
		}
		kc := string(dec)
		return kc, false, clusterMasks(kc, ""), nil
	case ClusterAuthToken:
		tok, err := s.decryptCredential(r.CredentialEnc, name)
		if err != nil {
			return "", false, nil, err
		}
		kc, err := synthesizeKubeconfig(r.ApiServer, r.CaCert, string(tok))
		if err != nil {
			return "", false, nil, fmt.Errorf("store: cluster %q: build kubeconfig: %w", name, err)
		}
		return kc, false, clusterMasks(kc, string(tok)), nil
	default:
		return "", false, nil, fmt.Errorf("store: cluster %q: unknown auth_type %q", name, r.AuthType)
	}
}

// clusterMasks is the log-redaction set for a resolved cluster: the
// whole kubeconfig blob (best effort for single-line cases), every
// sensitive scalar inside it, and — for token auth — the raw bearer
// token (which appears standalone, not only inside the kubeconfig).
// Deduplicated: the same value can surface in more than one field (the
// token path repeats the bearer token inside the synthesized config),
// and there's no reason to redact it twice.
func clusterMasks(kubeconfig, rawToken string) []string {
	masks := make([]string, 0, 4)
	if kubeconfig != "" {
		masks = append(masks, kubeconfig)
		masks = append(masks, sensitiveKubeconfigValues(kubeconfig)...)
	}
	if rawToken != "" {
		masks = append(masks, rawToken)
	}
	seen := make(map[string]struct{}, len(masks))
	out := masks[:0]
	for _, m := range masks {
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// sensitiveMinLen avoids masking trivially-short scalars (a 4-char
// value would blank unrelated log text).
const sensitiveMinLen = 8

// sensitiveKubeconfigKeys are the field names that carry a usable
// credential, matched case-insensitively at ANY depth. A "full
// kubeconfig" can nest a token well below users[].user.token — under
// auth-provider.config.{access-token,refresh-token,id-token} (OIDC),
// for instance — so the walk is recursive, not a fixed path. (exec-auth
// kubeconfigs are rejected at the cadastro, so their argv/env secrets
// never reach here.)
var sensitiveKubeconfigKeys = map[string]struct{}{
	"token":           {},
	"client-key-data": {},
	"password":        {},
	"client-secret":   {},
	"access-token":    {},
	"refresh-token":   {},
	"id-token":        {},
	"id_token":        {},
}

// sensitiveKubeconfigValues recursively walks a kubeconfig and returns
// every sensitive scalar (by key name, any depth). These are single-line
// values, so they survive the agent's line-by-line log redaction even
// when the multiline kubeconfig as a whole would not. Best-effort: a
// kubeconfig that doesn't parse yields no extra masks (the whole-blob
// mask still applies).
func sensitiveKubeconfigValues(kubeconfig string) []string {
	var root any
	if err := yaml.Unmarshal([]byte(kubeconfig), &root); err != nil {
		return nil
	}
	var out []string
	walkSensitive(root, &out)
	return out
}

// walkSensitive descends maps and slices, collecting string values whose
// key is in sensitiveKubeconfigKeys. Handles both map shapes yaml.v3 can
// produce for an untyped decode.
func walkSensitive(node any, out *[]string) {
	collect := func(k string, child any) {
		if _, hit := sensitiveKubeconfigKeys[strings.ToLower(k)]; hit {
			if s, ok := child.(string); ok && len(s) >= sensitiveMinLen {
				*out = append(*out, s)
			}
		}
		walkSensitive(child, out)
	}
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			collect(k, child)
		}
	case map[any]any:
		for k, child := range v {
			ks, _ := k.(string)
			collect(ks, child)
		}
	case []any:
		for _, child := range v {
			walkSensitive(child, out)
		}
	}
}

// validateAPIServerURL enforces that a token cluster's api_server is a
// safe, parseable endpoint: https only (a bearer token over http is
// cleartext), with a host and no embedded userinfo (credentials belong
// in the token field, never in the URL).
func validateAPIServerURL(raw string) error {
	return validateHTTPSURL(raw, "api_server")
}

// validateHTTPSURL is the shared https-only, host-present, no-userinfo
// check (used for the token api_server and the kubeconfig server before
// a connectivity probe). `field` names the offending input in the error.
func validateHTTPSURL(raw, field string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fmt.Errorf("%s URL is required", field)
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", field, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%s must be an https:// URL (got scheme %q) — a credential over http would be cleartext", field, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s must include a host", field)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not embed userinfo (user:pass@host)", field)
	}
	return nil
}

// rejectUnsupportedKubeconfig refuses a kubeconfig that relies on an
// `exec:` credential plugin — unsupported (gocdnext's plugin images
// don't ship the auth binaries) and a masking blind spot. Best-effort:
// a kubeconfig that doesn't parse is left to fail loudly at dispatch.
func rejectUnsupportedKubeconfig(kubeconfig string) error {
	var root any
	if err := yaml.Unmarshal([]byte(kubeconfig), &root); err != nil {
		return nil
	}
	if hasKey(root, "exec") {
		return errors.New("exec-based kubeconfig auth is not supported (the credential plugin isn't shipped) — use a static token/cert kubeconfig")
	}
	return nil
}

// hasKey reports whether a decoded YAML tree contains the given map key
// anywhere (case-insensitive).
func hasKey(node any, key string) bool {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if strings.EqualFold(k, key) || hasKey(child, key) {
				return true
			}
		}
	case map[any]any:
		for k, child := range v {
			if ks, ok := k.(string); ok && strings.EqualFold(ks, key) {
				return true
			}
			if hasKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if hasKey(child, key) {
				return true
			}
		}
	}
	return false
}

func (s *Store) decryptCredential(enc []byte, name string) ([]byte, error) {
	if s.authCipher == nil {
		return nil, fmt.Errorf("store: cluster %q: no cipher configured", name)
	}
	if len(enc) == 0 {
		return nil, fmt.Errorf("store: cluster %q: missing credential", name)
	}
	dec, err := s.authCipher.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("store: cluster %q: decrypt credential: %w", name, err)
	}
	return dec, nil
}

// sealClusterCredential encrypts the credential for kubeconfig/token,
// or returns nil for in_cluster.
func sealClusterCredential(cipher *crypto.Cipher, authType, credential string) ([]byte, error) {
	if authType == ClusterAuthInCluster {
		return nil, nil
	}
	if cipher == nil {
		return nil, errors.New("store: no cipher configured; cannot seal cluster credential")
	}
	enc, err := cipher.Encrypt([]byte(credential))
	if err != nil {
		return nil, fmt.Errorf("store: seal cluster credential: %w", err)
	}
	return enc, nil
}

// synthesizeKubeconfig builds a minimal single-context kubeconfig from
// a server URL, a CA PEM, and a bearer token. Marshalled via yaml.v3 so
// values are escaped safely (no string interpolation). A CA is
// mandatory — token auth NEVER falls back to insecure-skip-tls-verify
// (validateClusterInput already rejects a CA-less token cluster; this
// is the same invariant enforced again at synthesis, defense in depth).
func synthesizeKubeconfig(server string, caPEM []byte, token string) (string, error) {
	if len(caPEM) == 0 {
		return "", errors.New("token auth requires a ca_cert (refusing insecure-skip-tls-verify)")
	}
	clusterCfg := map[string]any{
		"server":                     server,
		"certificate-authority-data": base64.StdEncoding.EncodeToString(caPEM),
	}
	cfg := map[string]any{
		"apiVersion":      "v1",
		"kind":            "Config",
		"current-context": "gocdnext",
		"clusters":        []any{map[string]any{"name": "gocdnext", "cluster": clusterCfg}},
		"users":           []any{map[string]any{"name": "gocdnext", "user": map[string]any{"token": token}}},
		"contexts":        []any{map[string]any{"name": "gocdnext", "context": map[string]any{"cluster": "gocdnext", "user": "gocdnext"}}},
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func projectAllowed(allowed []string, projectID uuid.UUID) bool {
	if len(allowed) == 0 {
		return true // empty allow-list = any project
	}
	id := projectID.String()
	for _, a := range allowed {
		if a == id {
			return true
		}
	}
	return false
}

func normalizeProjectIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func clusterFromRow(id pgtype.UUID, name, desc, authType, apiServer string, ca []byte, allowed []string, createdBy string, created, updated pgtype.Timestamptz) Cluster {
	return Cluster{
		ID:              fromPgUUID(id),
		Name:            name,
		Description:     desc,
		AuthType:        authType,
		APIServer:       apiServer,
		CACert:          ca,
		AllowedProjects: append([]string(nil), allowed...),
		CreatedBy:       createdBy,
		CreatedAt:       pgTimePtr(created),
		UpdatedAt:       pgTimePtr(updated),
	}
}
