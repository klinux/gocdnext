package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// GCSConfig configures the GCS backend. One of CredentialsJSON or
// CredentialsFile must be non-empty for signed URLs to work — GCS
// signing needs an RSA private key that's not exposed when running
// under Workload Identity (which returns tokens, not keys). Prod
// deployments on GKE typically provision a dedicated "signer" service
// account with a JSON key just for URL signing; app credentials stay
// WIF-based.
//
// Endpoint is for fake-gcs-server in tests and is normally empty.
type GCSConfig struct {
	Bucket          string
	CredentialsFile string
	CredentialsJSON []byte
	Endpoint        string // fake-gcs-server e.g. "http://localhost:4443"
}

// GCSStore implements Store against Google Cloud Storage.
type GCSStore struct {
	client       *storage.Client
	bucket       string
	signerEmail  string
	signerKeyPEM []byte
}

// NewGCSStore wires the GCS client. If both CredentialsJSON and
// CredentialsFile are empty the client falls back to ADC — fine for
// Head/Get/Delete, but SignedURL will error because the library can't
// find a private key to sign with.
func NewGCSStore(ctx context.Context, cfg GCSConfig) (*GCSStore, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("artifacts: gcs: bucket is required")
	}

	credsJSON := cfg.CredentialsJSON
	if len(credsJSON) == 0 && cfg.CredentialsFile != "" {
		data, err := os.ReadFile(cfg.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("artifacts: gcs: read creds file: %w", err)
		}
		credsJSON = data
	}

	// Endpoint + creds are mutually exclusive for the SDK client
	// (DisableAuthentication rejects credential options). When a custom
	// endpoint is set we're talking to fake-gcs-server or similar — skip
	// credentials on the client but keep the signer key separately so
	// SignedURL still works in tests.
	var opts []option.ClientOption
	switch {
	case cfg.Endpoint != "":
		opts = append(opts, option.WithEndpoint(cfg.Endpoint), option.WithoutAuthentication())
	case len(credsJSON) > 0:
		opts = append(opts, option.WithCredentialsJSON(credsJSON))
	}

	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: gcs: new client: %w", err)
	}

	email, key, err := extractSigner(credsJSON)
	if err != nil {
		// Non-fatal: without a key we can still serve Head/Get/Delete;
		// SignedURL calls will error lazily.
		_ = err
	}
	return &GCSStore{
		client:       client,
		bucket:       cfg.Bucket,
		signerEmail:  email,
		signerKeyPEM: key,
	}, nil
}

// Client exposes the underlying client for tests.
func (g *GCSStore) Client() *storage.Client { return g.client }

// Bucket returns the configured bucket.
func (g *GCSStore) Bucket() string { return g.bucket }

func (g *GCSStore) SignedPutURL(ctx context.Context, key string, ttl time.Duration) (SignedURL, error) {
	return g.sign(ctx, key, ttl, "PUT")
}

func (g *GCSStore) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (SignedURL, error) {
	return g.sign(ctx, key, ttl, "GET")
}

func (g *GCSStore) sign(_ context.Context, key string, ttl time.Duration, method string) (SignedURL, error) {
	if g.signerEmail == "" || len(g.signerKeyPEM) == 0 {
		return SignedURL{}, errors.New("artifacts: gcs: signing key unavailable (configure CredentialsJSON/CredentialsFile)")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	opts := &storage.SignedURLOptions{
		GoogleAccessID: g.signerEmail,
		PrivateKey:     g.signerKeyPEM,
		Method:         method,
		Expires:        time.Now().Add(ttl),
		Scheme:         storage.SigningSchemeV4,
	}
	url, err := storage.SignedURL(g.bucket, key, opts)
	if err != nil {
		return SignedURL{}, fmt.Errorf("artifacts: gcs: sign %s: %w", method, err)
	}
	return SignedURL{URL: url, ExpiresAt: opts.Expires}, nil
}

func (g *GCSStore) Head(ctx context.Context, key string) (int64, error) {
	attrs, err := g.client.Bucket(g.bucket).Object(key).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("artifacts: gcs: head: %w", err)
	}
	return attrs.Size, nil
}

func (g *GCSStore) Delete(ctx context.Context, key string) error {
	err := g.client.Bucket(g.bucket).Object(key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("artifacts: gcs: delete: %w", err)
	}
	return nil
}

func (g *GCSStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	w := g.client.Bucket(g.bucket).Object(key).NewWriter(ctx)
	n, copyErr := io.Copy(w, r)
	closeErr := w.Close()
	if copyErr != nil {
		return 0, fmt.Errorf("artifacts: gcs: put body: %w", copyErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("artifacts: gcs: put close: %w", closeErr)
	}
	return n, nil
}

func (g *GCSStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := g.client.Bucket(g.bucket).Object(key).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: gcs: get: %w", err)
	}
	return rc, nil
}

// EnsureBucket creates the bucket if missing. Opt-in; not for prod GCS
// where buckets are provisioned out-of-band.
func (g *GCSStore) EnsureBucket(ctx context.Context, projectID string) error {
	_, err := g.client.Bucket(g.bucket).Attrs(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrBucketNotExist) {
		// Also check for googleapi 404 (different codepath when bucket
		// name collides with someone else's).
		var gErr *googleapi.Error
		if errors.As(err, &gErr) && gErr.Code != 404 {
			return fmt.Errorf("artifacts: gcs: check bucket: %w", err)
		}
	}
	if err := g.client.Bucket(g.bucket).Create(ctx, projectID, nil); err != nil {
		return fmt.Errorf("artifacts: gcs: create bucket %q: %w", g.bucket, err)
	}
	return nil
}

// extractSigner pulls the service account email + private_key PEM from
// the JSON credential blob. Empty blob = empty return with no error;
// caller gates signing behavior on the presence of a key.
func extractSigner(credsJSON []byte) (email string, keyPEM []byte, err error) {
	if len(credsJSON) == 0 {
		return "", nil, nil
	}
	var parsed struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(credsJSON, &parsed); err != nil {
		return "", nil, fmt.Errorf("artifacts: gcs: parse credential json: %w", err)
	}
	if parsed.ClientEmail == "" || parsed.PrivateKey == "" {
		return "", nil, errors.New("artifacts: gcs: credential json missing client_email/private_key")
	}
	return parsed.ClientEmail, []byte(parsed.PrivateKey), nil
}
