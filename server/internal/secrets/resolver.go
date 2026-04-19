// Package secrets carries the provider-agnostic contract for resolving
// project-scoped secrets at dispatch time. The default DB-backed Resolver
// wraps the at-rest cipher + store lookup we shipped in E1a/E1b; future
// adapters (HashiCorp Vault, AWS Secrets Manager, GCP Secret Manager) drop
// in here and the scheduler never has to know which one it's talking to.
package secrets

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Resolver turns a list of secret names into their plaintext values for a
// given project. Implementations are safe for concurrent use. A Resolver MUST
// treat unknown names as a silent omission (return the map without the name)
// so the scheduler's own diff can report a precise "not set on project" list
// instead of a generic "one of these failed".
type Resolver interface {
	Resolve(ctx context.Context, projectID uuid.UUID, names []string) (map[string]string, error)
}

// NopResolver is the no-secrets-subsystem default: it returns an empty map
// for any lookup. The scheduler uses this when GOCDNEXT_SECRET_KEY is unset,
// so jobs without `secrets:` still run fine. Jobs that DO declare secrets
// will surface "secrets not set on project: [...]" via the scheduler's diff.
type NopResolver struct{}

func (NopResolver) Resolve(context.Context, uuid.UUID, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

// DBResolver is the default implementation: decrypts values from the
// `secrets` table using the server's configured cipher.
type DBResolver struct {
	store  *store.Store
	cipher *crypto.Cipher
}

// NewDBResolver returns an error when either dependency is nil so the caller
// fails at boot instead of at first secret resolution.
func NewDBResolver(s *store.Store, c *crypto.Cipher) (*DBResolver, error) {
	if s == nil {
		return nil, fmt.Errorf("secrets: DBResolver needs a store")
	}
	if c == nil {
		return nil, fmt.Errorf("secrets: DBResolver needs a cipher")
	}
	return &DBResolver{store: s, cipher: c}, nil
}

func (r *DBResolver) Resolve(ctx context.Context, projectID uuid.UUID, names []string) (map[string]string, error) {
	return r.store.ResolveSecrets(ctx, r.cipher, projectID, names)
}
