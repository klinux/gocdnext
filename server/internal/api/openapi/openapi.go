// Package openapi serves the OpenAPI 3.1 spec that documents the
// public REST surface of the control plane. The YAML is embedded at
// build time so deployments don't need to ship the docs/ tree.
//
// Wired at /api/v1/openapi.yaml — public by design (the spec
// describes interfaces, never secrets).
package openapi

import (
	_ "embed"
	"net/http"
)

//go:embed gocdnext.yaml
var spec []byte

// Handler returns an http.Handler that serves the OpenAPI document.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(spec)
	})
}

// Spec returns the raw YAML bytes — useful for tests that want to
// assert on the served content without going through HTTP.
func Spec() []byte { return spec }
