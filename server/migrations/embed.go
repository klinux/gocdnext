package migrations

import "embed"

// FS exposes the goose migration files so they can be applied programmatically
// from tests or from the server bootstrap path (no external binary needed).
//
//go:embed *.sql
var FS embed.FS
