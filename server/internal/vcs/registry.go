// Package vcs is the in-process registry for VCS platform
// integrations (GitHub App today; GitLab/Bitbucket later).
// Consumers — checks.Reporter, projects.AutoRegister, webhook
// handler — call into the registry at request time instead of
// holding a *ghscm.AppClient directly, which lets the admin UI
// swap credentials at runtime without a server restart.
package vcs

import (
	"sync"
	"time"

	"github.com/google/uuid"

	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
)

// Source tracks where an integration came from, so the admin
// page can distinguish env-bootstrapped rows (read-only) from
// DB rows the UI can CRUD.
type Source string

const (
	SourceEnv Source = "env"
	SourceDB  Source = "db"
)

// Integration is the metadata shape surfaced to admin handlers.
// Secret material lives only inside the AppClient instance the
// registry caches alongside it.
type Integration struct {
	ID          uuid.UUID `json:"id,omitempty"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	DisplayName string    `json:"display_name,omitempty"`
	AppID       *int64    `json:"app_id,omitempty"`
	APIBase     string    `json:"api_base,omitempty"`
	Enabled     bool      `json:"enabled"`
	Source      Source    `json:"source"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// Registry is the hot-swappable container. Reads (GitHubApp,
// List) are RWMutex-protected; writes happen on admin CRUD +
// boot via Replace. Every getter returns safe-to-use copies.
type Registry struct {
	mu           sync.RWMutex
	githubApp    *ghscm.AppClient
	integrations []Integration
}

// New creates an empty registry. Callers populate it via Replace
// (both boot and reload go through the same entry point).
func New() *Registry {
	return &Registry{}
}

// Replace atomically swaps the registry contents. Pass the
// resolved AppClient for github_app (nil if unavailable) plus
// the metadata list the admin UI shows. Takes the list in
// stable insertion order — callers decide precedence (env first,
// then DB) and collisions resolve to "last seen wins" in the
// metadata; the AppClient argument is already resolved.
func (r *Registry) Replace(githubApp *ghscm.AppClient, integrations []Integration) {
	copy := append([]Integration(nil), integrations...)
	r.mu.Lock()
	r.githubApp = githubApp
	r.integrations = copy
	r.mu.Unlock()
}

// GitHubApp returns the primary GitHub App client, or nil if
// none is configured. Consumers guard every call: the registry
// is the ONLY place that decides whether the feature is
// available.
func (r *Registry) GitHubApp() *ghscm.AppClient {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.githubApp
}

// List returns the metadata slice. Safe to hand to the admin
// handler — no secret material leaks through this surface.
func (r *Registry) List() []Integration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Integration, len(r.integrations))
	copy(out, r.integrations)
	return out
}

// Len reports the enabled count (metadata-level). Zero = "no
// integration active", which the /settings/integrations UI
// renders as the empty state.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.integrations)
}
