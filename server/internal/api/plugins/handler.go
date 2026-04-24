// Package plugins exposes the read-only HTTP surface for the
// in-memory plugin catalog. The write path (loading manifests
// from disk) lives in `server/internal/plugins`; this package
// just marshals the loaded specs to JSON so the UI can render
// the docs page auto-generated from each `plugin.yaml`.
package plugins

import (
	"encoding/json"
	"log/slog"
	"net/http"

	plugcat "github.com/gocdnext/gocdnext/server/internal/plugins"
)

// Handler serves GET /api/v1/plugins. Nil-safe on the catalog —
// a server wired without a plugin dir still returns an empty
// list rather than 503, so the UI always has something to
// render (even if it's the "no plugins loaded" empty state).
type Handler struct {
	log     *slog.Logger
	catalog *plugcat.Catalog
}

func NewHandler(log *slog.Logger, catalog *plugcat.Catalog) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{log: log, catalog: catalog}
}

// inputDTO is the JSON shape one catalog Input surfaces to the
// UI. Kept separate from store.Input so a future column on the
// spec (e.g. `type: string | bool | secret`) doesn't leak
// through the API without an explicit opt-in.
type inputDTO struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

type exampleDTO struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	YAML        string `json:"yaml"`
}

type pluginDTO struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Category    string       `json:"category,omitempty"`
	Inputs      []inputDTO   `json:"inputs"`
	Examples    []exampleDTO `json:"examples,omitempty"`
}

type listResponse struct {
	Plugins []pluginDTO `json:"plugins"`
}

// List handles GET /api/v1/plugins. Returns the catalog in
// sorted order (same order Catalog.Names yields) so the UI list
// is stable across refreshes. Inputs per plugin are emitted
// sorted by name for the same reason.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := listResponse{Plugins: []pluginDTO{}}
	if h.catalog != nil {
		for _, s := range h.catalog.Specs() {
			resp.Plugins = append(resp.Plugins, toPluginDTO(s))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func toPluginDTO(s plugcat.Spec) pluginDTO {
	inputs := make([]inputDTO, 0, len(s.Inputs))
	// Walk sorted so the UI shows inputs in a predictable order
	// (doc-style), not whatever map-iteration order Go chose.
	names := make([]string, 0, len(s.Inputs))
	for n := range s.Inputs {
		names = append(names, n)
	}
	sortStrings(names)
	for _, n := range names {
		in := s.Inputs[n]
		inputs = append(inputs, inputDTO{
			Name:        n,
			Required:    in.Required,
			Default:     in.Default,
			Description: in.Description,
		})
	}
	examples := make([]exampleDTO, 0, len(s.Examples))
	for _, ex := range s.Examples {
		examples = append(examples, exampleDTO{
			Name:        ex.Name,
			Description: ex.Description,
			YAML:        ex.YAML,
		})
	}
	return pluginDTO{
		Name:        s.Name,
		Description: s.Description,
		Category:    s.Category,
		Inputs:      inputs,
		Examples:    examples,
	}
}
