package scheduler

import (
	"context"
	"strings"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	bctoken "github.com/gocdnext/gocdnext/server/internal/buildcache"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// gradlePluginRepo is the EXACT repository basename of the gocdnext gradle
// plugin — the only runtime that materialises the build-cache init script.
const gradlePluginRepo = "gocdnext-plugin-gradle"

// usesGradlePlugin reports whether any of the job's plugin tasks run the gradle
// plugin, so auto-wiring is injected only where it's actually consumed. It
// compares the image's repository BASENAME exactly (not a substring) so a
// look-alike like "ghcr.io/evil/gocdnext-plugin-gradle-backdoor" is not
// mistaken for the real plugin and handed a cache credential.
func usesGradlePlugin(a *gocdnextv1.JobAssignment) bool {
	for _, t := range a.GetTasks() {
		if imageRepoBasename(t.GetPlugin().GetImage()) == gradlePluginRepo {
			return true
		}
	}
	return false
}

// imageRepoBasename extracts the last path segment of an image reference's
// repository, stripping any @digest and :tag. Handles a registry host:port
// prefix because the tag is only the ':' AFTER the last '/'.
//
//	ghcr.io/klinux/gocdnext-plugin-gradle@sha256:… -> gocdnext-plugin-gradle
//	host:5000/klinux/gocdnext-plugin-gradle:v1     -> gocdnext-plugin-gradle
func imageRepoBasename(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, '/'); i >= 0 {
		image = image[i+1:]
	}
	if i := strings.IndexByte(image, ':'); i >= 0 {
		image = image[:i]
	}
	return image
}

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
	// Only auto-wire jobs that actually run the gocdnext gradle plugin — that's
	// the only place the init script gets written, so injecting the credential
	// anywhere else just exposes it (and costs DB lookups) for nothing.
	if !usesGradlePlugin(a) {
		return
	}
	// gen is the project's current credential generation (token hash); "" means
	// the cache is disabled. Minting with it binds the token to this generation,
	// so a later rotate/disable invalidates it.
	_, gen, err := s.store.ProjectCacheAuthBySlug(ctx, run.ProjectSlug)
	if err != nil {
		s.log.Warn("scheduler: build cache lookup", "project", run.ProjectSlug, "err", err)
		return
	}
	if gen == "" {
		return
	}
	// Write (push) access only for runs that own the shared lane — i.e. a
	// default-branch build (the same rule PR-safe scoping uses, #318). PR /
	// feature-branch / tag runs get a READ-ONLY token so an untrusted run can
	// neither poison the shared cache nor exhaust its storage; they still read
	// the default branch's entries. A scope-resolve error falls back to
	// read-only (the safe direction).
	write := false
	if scope, err := s.store.ResolveCacheScope(ctx, run.ID, run.ProjectID); err == nil {
		write = scope == store.CacheScopeShared
	} else {
		s.log.Warn("scheduler: build cache scope resolve", "run_id", run.ID, "err", err)
	}

	token := bctoken.MintToken(run.ProjectID, write, s.buildCacheTokenTTL, s.buildCacheSignKey, []byte(gen))
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
	// Push only when this run holds a write token; read-only runs still pull.
	if write {
		a.Env["GOCDNEXT_BUILD_CACHE_PUSH"] = "true"
	} else {
		a.Env["GOCDNEXT_BUILD_CACHE_PUSH"] = "false"
	}
	// The token is a bearer credential — mask it so it never survives into a
	// log line (same discipline as OIDC id_tokens and clone tokens).
	a.LogMasks = append(a.LogMasks, token)
}
