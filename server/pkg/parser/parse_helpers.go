package parser

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func defaultEvents(ev []string) []string {
	if len(ev) == 0 {
		return []string{"push"}
	}
	return ev
}

// pipelineEvents enumerates the values accepted in top-level
// `when.event:` — webhook-driven (push, pull_request, tag), trigger-
// based (manual, cron), and dependency-based (upstream). Adding to
// this set is the explicit handshake operators trip when wiring a
// new trigger type, vs. the silent-accept-and-never-fire bug a free-
// text list lets through.
var pipelineEvents = map[string]struct{}{
	"push":         {},
	"pull_request": {},
	"tag":          {},
	"manual":       {},
	"cron":         {},
	"upstream":     {},
}

// gitMaterialEvents are the subset that mean anything to a git
// material's `on:` filter — only SCM events can actually arrive on
// that material. cron/manual/upstream don't have an "on a git
// material" semantic; `on: [cron]` was always a no-op + a typo.
var gitMaterialEvents = map[string]struct{}{
	"push":         {},
	"pull_request": {},
	"tag":          {},
}

func validatePipelineEvents(ev []string) error {
	for _, e := range ev {
		if _, ok := pipelineEvents[e]; !ok {
			return fmt.Errorf("unknown event %q (accepted: push, pull_request, tag, manual, cron, upstream)", e)
		}
	}
	return nil
}

func validateGitMaterialEvents(on []string) error {
	for _, e := range on {
		if _, ok := gitMaterialEvents[e]; !ok {
			return fmt.Errorf("unknown event %q in `on:` (accepted: push, pull_request, tag)", e)
		}
	}
	return nil
}

// outputAliasRE is the allowed character set for an `outputs:` map
// key (the YAML alias the operator types). Same shape as a shell
// identifier so substitution refs `${{ needs.X.outputs.<alias> }}`
// parse predictably and the alias can appear in a downstream env
// var name without escaping. ^[a-z] forces lowercase-leading per
// the gocdnext YAML convention.
var outputAliasRE = regexp.MustCompile(`^[a-z][a-zA-Z0-9_-]*$`)

// outputEnvRE is the allowed character set for the RIGHT-hand
// value of an `outputs:` map entry — the plugin's env-var name
// written to $GOCDNEXT_OUTPUT_FILE. Standard POSIX env-var-name
// shape: starts with letter/underscore, then alphanumerics +
// underscores. No lowercase requirement because the operator
// might be mirroring a third-party plugin's naming convention
// (NEXT, PROMOTED_DIGEST, etc.).
var outputEnvRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateOutputsDeclaration enforces the alias / env-name shape +
// a soft per-job limit so a misbehaving pipeline can't declare an
// open-ended outputs blob. The 64KB cap on actual output VALUES
// applies at agent + server layers; here we just bound the
// declaration count (operator-facing) so a 10000-entry block
// surfaces at apply rather than at dispatch.
func validateOutputsDeclaration(jobName string, outputs map[string]string) error {
	const maxOutputs = 64
	if len(outputs) > maxOutputs {
		return fmt.Errorf("job %q: outputs declares %d entries, cap is %d (open an issue if you legitimately need more)",
			jobName, len(outputs), maxOutputs)
	}
	for alias, envName := range outputs {
		if !outputAliasRE.MatchString(alias) {
			return fmt.Errorf("job %q: outputs alias %q must match %s — typically lowercase + dashes (e.g. `next`, `image-digest`)",
				jobName, alias, outputAliasRE.String())
		}
		if envName == "" {
			return fmt.Errorf("job %q: outputs alias %q maps to an empty env-var name — must name the variable the plugin writes to $GOCDNEXT_OUTPUT_FILE",
				jobName, alias)
		}
		if !outputEnvRE.MatchString(envName) {
			return fmt.Errorf("job %q: outputs[%s] env-var name %q must match %s — POSIX env-var shape (e.g. NEXT, PROMOTED_DIGEST)",
				jobName, alias, envName, outputEnvRE.String())
		}
	}
	return nil
}

// toService validates a service spec and derives a default name
// when omitted. image is mandatory — a service without one can't
// start. Name defaults to the image's short form (repository
// basename, tag stripped) so `image: postgres:16-alpine` implies
// `name: postgres` without extra YAML. Duplicate names across
// the pipeline would collide on the docker network alias; that
// check lives in ApplyProject where all services are visible
// together.
// notificationTriggers is the closed set of `on:` values. Keep
// in sync with domain.NotificationTrigger constants.
var notificationTriggers = map[string]domain.NotificationTrigger{
	"failure":  domain.NotifyOnFailure,
	"success":  domain.NotifyOnSuccess,
	"always":   domain.NotifyOnAlways,
	"canceled": domain.NotifyOnCanceled,
}

func toNotification(idx int, n NotificationSpec) (domain.Notification, error) {
	on := strings.TrimSpace(strings.ToLower(n.On))
	trig, ok := notificationTriggers[on]
	if !ok {
		return domain.Notification{}, fmt.Errorf(
			"notifications[%d]: unknown on %q (allowed: failure, success, always, canceled)",
			idx, n.On,
		)
	}
	if strings.TrimSpace(n.Uses) == "" {
		return domain.Notification{}, fmt.Errorf("notifications[%d]: `uses:` is required", idx)
	}
	return domain.Notification{
		On:      trig,
		Uses:    strings.TrimSpace(n.Uses),
		With:    cloneStrMap(n.With),
		Secrets: append([]string(nil), n.Secrets...),
	}, nil
}

func cloneStrMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toService(s ServiceSpec) (domain.Service, error) {
	if strings.TrimSpace(s.Image) == "" {
		return domain.Service{}, fmt.Errorf("service: image is required")
	}
	name := strings.TrimSpace(s.Name)
	if name == "" {
		name = defaultServiceNameFromImage(s.Image)
	}
	if name == "" {
		return domain.Service{}, fmt.Errorf("service: couldn't derive name from image %q; set `name:` explicitly", s.Image)
	}
	return domain.Service{
		Name:    name,
		Image:   s.Image,
		Env:     s.Env,
		Command: append([]string(nil), s.Command...),
	}, nil
}

// defaultServiceNameFromImage picks a dns-label-friendly name from
// a container image reference. "postgres:16-alpine" → "postgres",
// "registry.local/foo/bar:1" → "bar". Strips registry+repo path
// and tag.
func defaultServiceNameFromImage(image string) string {
	s := image
	// Strip tag.
	if i := strings.LastIndex(s, ":"); i >= 0 {
		// Colons in host:port (registry) survive since they appear
		// BEFORE any slash — only strip when the last colon is
		// after the last slash.
		lastSlash := strings.LastIndex(s, "/")
		if i > lastSlash {
			s = s[:i]
		}
	}
	// Strip registry + repo path.
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// canonicalArtifactPath strips trailing slashes so `dist` and
// `dist/` collapse to the same key when deduping artifact entries.
// Mirrors store.NormalizeArtifactPath (kept inline rather than
// importing — the parser shouldn't depend on the storage layer).
// Only the trailing slash is touched — we deliberately do NOT
// resolve `.`/`..` or otherwise rewrite the path, since the
// agent's tar/untar loop preserves operator-declared shape verbatim.
func canonicalArtifactPath(p string) string {
	for len(p) > 1 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}
