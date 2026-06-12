package webhook

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// PRFilesFetcher resolves the changed-file list of a pull request via
// the provider's API (push payloads embed file lists; PR payloads
// don't). The scm_source carries the credential reference the
// implementation resolves. `known=false` means the set couldn't be
// fetched completely — no credentials, API error, or past the
// pagination cap — and the caller MUST fail open. Implemented by
// configsync.PRFiles.
type PRFilesFetcher interface {
	PRChangedFiles(ctx context.Context, source store.SCMSource, number int) (files []string, known bool)
}

// WithPRFilesFetcher opts the handler into `when.paths` filtering on
// pull_request events. Nil (default) keeps PR path filtering failing
// open — pipelines with paths still run on every PR.
func (h *Handler) WithPRFilesFetcher(f PRFilesFetcher) *Handler {
	h.prFiles = f
	return h
}

// pathsMatch reports whether a material with `globs` should fire for
// a delivery whose changed files are `files`. Decision table:
//
//	no globs      → fire (feature not in use)
//	!known        → fire (FAIL OPEN — a partial set must never
//	                 suppress a legitimate run; extra runs are noise,
//	                 missing CI is an incident)
//	any file matches any glob → fire
//	otherwise     → filtered out
func pathsMatch(globs, files []string, known bool) bool {
	if len(globs) == 0 {
		return true
	}
	if !known {
		return true
	}
	for _, f := range files {
		for _, g := range globs {
			// Patterns are parse-time validated; Match only errors on
			// bad patterns, so the error path is unreachable here.
			if ok, _ := doublestar.Match(g, f); ok {
				return true
			}
		}
	}
	return false
}

// anyMaterialHasPaths reports whether at least one material declares
// `paths:` globs — the cheap pre-check that gates the PR files-API
// round-trip.
func anyMaterialHasPaths(materials []store.Material) bool {
	for _, m := range materials {
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			continue
		}
		if len(cfg.Paths) > 0 {
			return true
		}
	}
	return false
}

// filterMaterialsByPaths drops materials whose `paths:` don't match
// the delivery's changed files. Returns the surviving materials and
// the number filtered out (for the response body / logs). Materials
// with undecodable config are kept — the fan-out path already owns
// that failure mode.
func filterMaterialsByPaths(
	log *slog.Logger,
	materials []store.Material,
	files []string,
	known bool,
	provider, delivery string,
) (kept []store.Material, filtered int) {
	kept = make([]store.Material, 0, len(materials))
	for _, m := range materials {
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			kept = append(kept, m)
			continue
		}
		if pathsMatch(cfg.Paths, files, known) {
			kept = append(kept, m)
			continue
		}
		filtered++
		log.Info(provider+" webhook: material filtered by when.paths",
			"delivery", delivery, "material_id", m.ID,
			"globs", cfg.Paths, "changed_files", len(files))
	}
	return kept, filtered
}
