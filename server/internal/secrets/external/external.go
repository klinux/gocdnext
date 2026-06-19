// Package external defines the contract for fetching a single secret value
// from an external store (HashiCorp Vault / GCP Secret Manager / AWS Secrets
// Manager), used by the composite resolver for reference-model secrets.
package external

import (
	"context"
	"errors"
)

// ErrSecretNotFound is returned when path/key resolves to nothing. The
// composite resolver treats it as silent omission (matching the DB
// resolver's "unknown name dropped" contract) so the scheduler's diff
// reports a precise "secrets not set on project" list. Any OTHER error
// (auth, network, malformed payload) propagates so the dispatch fails loud
// — a configured-but-broken backend must not look like "secret absent".
var ErrSecretNotFound = errors.New("external: secret not found")

// Backend fetches one secret value. path/key come verbatim from the secret
// entry's ref_path / ref_key. Implementations must be safe for concurrent
// use and must NOT log the returned value.
type Backend interface {
	// Fetch returns the secret value at path[/key], or ErrSecretNotFound.
	Fetch(ctx context.Context, path, key string) (string, error)
	// Name is the source token this backend serves: "vault" | "gcp" | "aws".
	Name() string
	// HealthCheck does a lightweight reachability+auth probe (no secret
	// value involved) so the admin "Test connection" flow can validate a
	// backend's config/credentials before a job depends on it. nil = ok.
	HealthCheck(ctx context.Context) error
}
