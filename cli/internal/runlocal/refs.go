package runlocal

import "github.com/gocdnext/gocdnext/server/pkg/refs"

// run-local resolves `${{ NAME }}` / `${VAR}` through the SAME core
// the scheduler's dispatch uses (server/pkg/refs, #44) — the local
// copy that used to mirror it by hand is gone, so a pipeline that
// resolves clean in `run-local` resolves clean for real, and vice
// versa. needs.* outputs don't exist locally, so only the plain-ref +
// shell-var passes are aliased here (the strict pass errors loudly on
// a `${{ needs.* }}` body, which is the right run-local behaviour).
var (
	substituteRefs      = refs.SubstituteRefs
	substituteShellVars = refs.SubstituteShellVars
)
