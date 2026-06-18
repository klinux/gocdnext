package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"gopkg.in/yaml.v3"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrClusterNotFound is returned when a cluster id/name doesn't resolve.
var ErrClusterNotFound = errors.New("store: cluster not found")

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
		// Credential required on insert; on update the preserve
		// sentinel stands in for "keep the existing one".
		if in.Credential == "" && !(isUpdate) {
			return fmt.Errorf("store: cluster %q: auth_type %q needs a credential", in.Name, in.AuthType)
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
	if err := s.q.UpdateCluster(ctx, db.UpdateClusterParams{
		ID:              pgUUID(id),
		Name:            in.Name,
		Description:     in.Description,
		AuthType:        in.AuthType,
		ApiServer:       in.APIServer,
		CaCert:          in.CACert,
		CredentialEnc:   credEnc,
		AllowedProjects: normalizeProjectIDs(in.AllowedProjects),
	}); err != nil {
		return fmt.Errorf("store: update cluster %s: %w", id, err)
	}
	return nil
}

// DeleteCluster removes the row. Caller MUST have checked usage first.
func (s *Store) DeleteCluster(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM clusters WHERE id = $1`, pgUUID(id))
	if err != nil {
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
// injection as PLUGIN_KUBECONFIG. in_cluster returns ("", true, nil) —
// no kubeconfig; the job pod's mounted SA is used. Uses the store's
// authCipher (runtime path), not a passed cipher.
func (s *Store) ResolveClusterForDispatch(ctx context.Context, projectID uuid.UUID, name string) (kubeconfig string, inCluster bool, err error) {
	r, err := s.q.GetClusterForDispatch(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrClusterNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("store: resolve cluster %q: %w", name, err)
	}
	if !projectAllowed(r.AllowedProjects, projectID) {
		return "", false, fmt.Errorf("store: project not authorized for cluster %q", name)
	}
	switch r.AuthType {
	case ClusterAuthInCluster:
		return "", true, nil
	case ClusterAuthKubeconfig:
		dec, err := s.decryptCredential(r.CredentialEnc, name)
		if err != nil {
			return "", false, err
		}
		return string(dec), false, nil
	case ClusterAuthToken:
		tok, err := s.decryptCredential(r.CredentialEnc, name)
		if err != nil {
			return "", false, err
		}
		kc, err := synthesizeKubeconfig(r.ApiServer, r.CaCert, string(tok))
		if err != nil {
			return "", false, fmt.Errorf("store: cluster %q: build kubeconfig: %w", name, err)
		}
		return kc, false, nil
	default:
		return "", false, fmt.Errorf("store: cluster %q: unknown auth_type %q", name, r.AuthType)
	}
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
// a server URL, an optional CA PEM, and a bearer token. Marshalled via
// yaml.v3 so values are escaped safely (no string interpolation).
func synthesizeKubeconfig(server string, caPEM []byte, token string) (string, error) {
	clusterCfg := map[string]any{"server": server}
	if len(caPEM) > 0 {
		clusterCfg["certificate-authority-data"] = base64.StdEncoding.EncodeToString(caPEM)
	} else {
		clusterCfg["insecure-skip-tls-verify"] = true
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
