// Package auth wires the HTTP session + identity layer on the
// control plane. It abstracts over IdPs so GitHub (plain OAuth2 +
// REST) and Google / Keycloak / arbitrary OIDC (all via go-oidc)
// look the same to the handler: authorize → callback → claims.
//
// The package intentionally does NOT talk to the database; store
// calls live in the handler so the tests can plug a fake provider
// without standing up a pool.
package auth

import (
	"context"
	"errors"
	"sync"
)

// ProviderName is a short stable string we key users + config on.
// Kept as a type so typos at call sites get caught by the compiler.
type ProviderName string

const (
	ProviderGitHub   ProviderName = "github"
	ProviderGoogle   ProviderName = "google"
	ProviderKeycloak ProviderName = "keycloak"
	// ProviderOIDC is the catch-all for corporate SSO setups that
	// don't match any of the hard-coded providers above.
	ProviderOIDC ProviderName = "oidc"
)

// Claims is the normalized user profile every provider must
// produce. Subject is the provider-specific stable user id — we
// persist it as users.external_id and never show it in the UI.
type Claims struct {
	Subject   string
	Email     string
	Name      string
	AvatarURL string
}

// ErrClaimsMissing is raised when the IdP accepted the code
// exchange but didn't return enough profile info to create a user
// row (no email, no subject). Handlers turn this into a 502 with a
// message pointing at the IdP config.
var ErrClaimsMissing = errors.New("auth: provider returned incomplete claims")

// Provider is the minimal interface the handler depends on. Every
// flow is: AuthorizeURL → redirect → callback → Exchange.
//
// DisplayName + ButtonLabel fuel the login page; the rest of the
// methods drive the OAuth/OIDC dance.
type Provider interface {
	Name() ProviderName
	DisplayName() string

	// AuthorizeURL returns the URL to redirect the browser to. The
	// `state` token is opaque to the provider but our own store
	// validates it on the callback; `nonce` is forwarded to OIDC
	// providers and ignored by GitHub.
	AuthorizeURL(state, nonce string) string

	// Exchange completes the code → profile dance. Implementations
	// are free to call userinfo or decode the id_token — the
	// handler only needs normalized Claims out the other side.
	Exchange(ctx context.Context, code, state, nonce string) (Claims, error)
}

// Registry is the set of providers enabled right now. Contents are
// swappable (see Replace) so the admin UI can rebuild the set after
// a CRUD operation without restarting the server. Iteration order
// is stable (insertion order) so the login page renders buttons in
// a deterministic sequence.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
	byName    map[ProviderName]Provider
}

// NewRegistry seeds a registry. A nil slice is valid: the login
// page just says "no identity providers configured."
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{}
	r.Replace(providers...)
	return r
}

// Replace atomically swaps the provider set. Safe to call
// concurrently with Get/List/Len from the request path.
//
// Name-collision policy: later entries win. First-seen position
// is preserved so passing Replace(env..., db...) keeps env order
// in the login page while letting DB rows override the values.
func (r *Registry) Replace(providers ...Provider) {
	byName := make(map[ProviderName]Provider, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		byName[p.Name()] = p
	}
	seen := make(map[ProviderName]bool, len(byName))
	ordered := make([]Provider, 0, len(byName))
	for _, p := range providers {
		if p == nil {
			continue
		}
		name := p.Name()
		if seen[name] {
			continue
		}
		seen[name] = true
		ordered = append(ordered, byName[name])
	}
	r.mu.Lock()
	r.providers = ordered
	r.byName = byName
	r.mu.Unlock()
}

// Get returns the provider registered under `name`, or nil when
// absent. Handlers should treat nil as "unknown provider" → 404.
func (r *Registry) Get(name ProviderName) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byName[name]
}

// List returns the providers in insertion order.
func (r *Registry) List() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, len(r.providers))
	copy(out, r.providers)
	return out
}

// Len reports how many providers are enabled. Zero = auth effectively
// disabled from the login page's perspective.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}
