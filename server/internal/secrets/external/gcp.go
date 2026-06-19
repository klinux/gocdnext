package external

import (
	"context"
	"errors"
	"fmt"
	"strings"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GCPConfig is the connection config. Authentication uses Application
// Default Credentials — workload identity on GKE (keyless, recommended), or
// a key file pointed at by the standard GOOGLE_APPLICATION_CREDENTIALS env
// var. We deliberately don't accept raw JSON creds inline (deprecated, and a
// static key in env is the thing keyless auth exists to avoid).
type GCPConfig struct {
	Project string
}

// GCPBackend reads from GCP Secret Manager. ref_path is the secret id (e.g.
// "gh-token"); a full or partial resource name ("projects/<p>/secrets/<id>")
// is also accepted and parsed. ref_key is the version ("latest" when empty).
type GCPBackend struct {
	client  *secretmanager.Client
	project string
}

// NewGCPBackend builds the client via ADC (fail-fast at boot).
func NewGCPBackend(ctx context.Context, cfg GCPConfig) (*GCPBackend, error) {
	if cfg.Project == "" {
		return nil, errors.New("gcp: project is required")
	}
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: new client: %w", err)
	}
	return &GCPBackend{client: client, project: cfg.Project}, nil
}

func (b *GCPBackend) Name() string { return "gcp" }

// Close releases the underlying gRPC client. Called when the registry rebuilds
// the backend after a config change so connections don't leak.
func (b *GCPBackend) Close() error { return b.client.Close() }

// HealthCheck lists one secret in the project to validate ADC/workload-
// identity reach the Secret Manager API (no secret value involved). An empty
// project (iterator.Done) is a healthy "reachable, nothing to list".
func (b *GCPBackend) HealthCheck(ctx context.Context) error {
	it := b.client.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{
		Parent:   fmt.Sprintf("projects/%s", b.project),
		PageSize: 1,
	})
	if _, err := it.Next(); err != nil && err != iterator.Done {
		return fmt.Errorf("gcp: health check: %w", err)
	}
	return nil
}

// gcpResourceName builds the version resource name accessed by Fetch. path may
// be a bare secret id ("gh-token") or a full/partial resource name
// ("projects/<p>/secrets/gh-token", optionally with a trailing
// "/versions/..."). The version always comes from key (empty → "latest"); a
// version baked into the path is stripped so the suffix isn't duplicated.
//
// _PROJECT is a tenancy boundary, not just a default: a full resource name
// that targets a different project is rejected, so a reference can't reach
// beyond the project this server is scoped to (defense in depth — IAM is the
// outer boundary, this is the inner one).
func gcpResourceName(project, path, key string) (string, error) {
	version := key
	if version == "" {
		version = "latest"
	}
	if strings.HasPrefix(path, "projects/") {
		base := path
		if i := strings.Index(base, "/versions/"); i >= 0 {
			base = base[:i]
		}
		parts := strings.Split(base, "/")
		if len(parts) != 4 || parts[0] != "projects" || parts[2] != "secrets" || parts[1] == "" || parts[3] == "" {
			return "", fmt.Errorf("gcp: malformed secret resource name %q (want projects/<project>/secrets/<id>)", path)
		}
		if parts[1] != project {
			return "", fmt.Errorf("gcp: reference targets project %q but this server is configured for %q", parts[1], project)
		}
		return fmt.Sprintf("%s/versions/%s", base, version), nil
	}
	return fmt.Sprintf("projects/%s/secrets/%s/versions/%s", project, path, version), nil
}

// Fetch accesses projects/<project>/secrets/<path>/versions/<key|latest>.
// path may be a bare secret id ("gh-token") or a full resource name
// ("projects/<p>/secrets/gh-token", with or without a trailing
// "/versions/..."). The version always comes from key — we own that segment —
// so a version baked into the path is stripped to avoid a duplicated suffix.
func (b *GCPBackend) Fetch(ctx context.Context, path, key string) (string, error) {
	name, err := gcpResourceName(b.project, path, key)
	if err != nil {
		return "", err
	}
	resp, err := b.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", ErrSecretNotFound
		}
		return "", fmt.Errorf("gcp: access %s: %w", name, err)
	}
	if resp.GetPayload() == nil {
		return "", ErrSecretNotFound
	}
	return string(resp.GetPayload().GetData()), nil
}
