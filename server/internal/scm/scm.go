// Package scm defines the provider-agnostic types shared across
// every SCM integration (GitHub, GitLab, Bitbucket, future
// Gitea/Bitbucket-Server/etc). Concrete implementations live in
// sibling subpackages and all speak these types at their
// boundary so configsync + webhook can treat them uniformly.
package scm

import "errors"

// RawFile is one YAML file fetched from a repo's config folder —
// name (basename, e.g. "web.yaml") + full content (utf-8 text,
// base64-decoded where the provider API returned it encoded).
// Path stripped because the parser only cares about filename.
type RawFile struct {
	Name    string
	Content string
}

// ErrFolderNotFound signals the repo was reachable but the
// config folder (default ".gocdnext") didn't exist at the given
// ref. Provider impls wrap it via fmt.Errorf("%w: ...", ErrFolderNotFound, ...)
// so callers match with errors.Is. Distinct from transport /
// auth failures — "folder absent" is a soft state the UI
// surfaces as a warning, anything else is an error.
var ErrFolderNotFound = errors.New("scm: config folder not found")
