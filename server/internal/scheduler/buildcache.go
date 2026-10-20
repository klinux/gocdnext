package scheduler

import (
	"context"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	bctoken "github.com/gocdnext/gocdnext/server/internal/buildcache"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// injectBuildCache auto-wires the embedded Gradle build cache for a job whose
// project has it enabled: it injects the cache URL + a freshly minted, masked
// per-run token into the assignment env, so the agent can configure Gradle's
// HttpBuildCache with zero manual setup. No-op when auto-wiring isn't
// configured (no signing key / public base) or the project hasn't enabled it.
//
// Non-fatal by design: a lookup error just skips auto-wiring (the build runs
// without the remote cache) rather than failing dispatch — the cache is an
// accelerator, never a correctness dependency.
func (s *Scheduler) injectBuildCache(ctx context.Context, run store.RunForDispatch, a *gocdnextv1.JobAssignment) {
	if len(s.buildCacheSignKey) == 0 || s.buildCachePublicBase == "" || a == nil {
		return
	}
	enabled, err := s.store.GetProjectCacheEnabledBySlug(ctx, run.ProjectSlug)
	if err != nil {
		s.log.Warn("scheduler: build cache enabled lookup", "project", run.ProjectSlug, "err", err)
		return
	}
	if !enabled {
		return
	}
	token := bctoken.MintToken(run.ProjectID, s.buildCacheTokenTTL, s.buildCacheSignKey)
	if token == "" {
		return
	}
	url := s.buildCachePublicBase + "/api/v1/buildcache/" + run.ProjectSlug + "/"

	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.Env["GOCDNEXT_BUILD_CACHE_URL"] = url
	a.Env["GOCDNEXT_BUILD_CACHE_USER"] = "gocdnext"
	a.Env["GOCDNEXT_BUILD_CACHE_PASSWORD"] = token
	// The token is a bearer credential — mask it so it never survives into a
	// log line (same discipline as OIDC id_tokens and clone tokens).
	a.LogMasks = append(a.LogMasks, token)
}
