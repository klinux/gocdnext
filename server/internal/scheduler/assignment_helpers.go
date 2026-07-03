package scheduler

import (
	"encoding/json"
	"fmt"
	"strings"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
	"github.com/google/uuid"
)

// tolerationsToProto maps the store-side Toleration list (validated
// + normalised at write time) to the proto wire shape. Returns nil
// on empty input so the wire stays minimal — engines treat absent +
// empty list identically.
//
// TolerationSeconds is COPIED into a fresh pointer rather than
// aliased: BuildAssignment should produce a wire object independent
// of the caller's input slice, so a later mutation of the store's
// Toleration (e.g. a future cache that mutates in place) can't
// retroactively change a JobAssignment already shipped to an agent.
func tolerationsToProto(in []store.Toleration) []*gocdnextv1.Toleration {
	if len(in) == 0 {
		return nil
	}
	out := make([]*gocdnextv1.Toleration, len(in))
	for i, t := range in {
		var seconds *int64
		if t.TolerationSeconds != nil {
			v := *t.TolerationSeconds
			seconds = &v
		}
		out[i] = &gocdnextv1.Toleration{
			Key:               t.Key,
			Operator:          t.Operator,
			Value:             t.Value,
			Effect:            t.Effect,
			TolerationSeconds: seconds,
		}
	}
	return out
}

// copyStringMap returns a fresh copy of the input — nil-tolerant.
// Used for the JobAssignment.Outputs field so a later mutation of
// the parsed-pipeline cache doesn't leak into in-flight assignments.
func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// resourceRequirements maps the resolved domain ResourceSpec to its
// proto twin. Returns nil when nothing is set so the wire stays
// minimal — engines treat absent + all-empty identically.
func resourceRequirements(r domain.ResourceSpec) *gocdnextv1.ResourceRequirements {
	if r.IsZero() {
		return nil
	}
	return &gocdnextv1.ResourceRequirements{
		CpuRequest:    r.Requests.CPU,
		CpuLimit:      r.Limits.CPU,
		MemoryRequest: r.Requests.Memory,
		MemoryLimit:   r.Limits.Memory,
	}
}

// JobSecretsFromDefinition returns the list of secret names a job declares,
// by parsing the stored pipeline definition JSONB. Exported so the scheduler
// can resolve secrets before calling BuildAssignment.
func JobSecretsFromDefinition(definition []byte, jobName string) ([]string, error) {
	jobDef, err := jobDefFromDefinition(definition, jobName)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), jobDef.Secrets...), nil
}

// JobTagsFromDefinition returns the list of required agent tags for a job.
// Exported so the scheduler can filter live sessions before picking one.
func JobTagsFromDefinition(definition []byte, jobName string) ([]string, error) {
	jobDef, err := jobDefFromDefinition(definition, jobName)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), jobDef.Tags...), nil
}

// JobArtifactDepsFromDefinition returns the `needs_artifacts:` entries
// for a job, decoded into domain.ArtifactDep. Scheduler calls this
// ahead of BuildAssignment so it can fetch the upstream artefact rows
// and sign download URLs to embed in the assignment.
func JobArtifactDepsFromDefinition(definition []byte, jobName string) ([]domain.ArtifactDep, error) {
	jobDef, err := jobDefFromDefinition(definition, jobName)
	if err != nil {
		return nil, err
	}
	return append([]domain.ArtifactDep(nil), jobDef.ArtifactDeps...), nil
}

func jobDefFromDefinition(definition []byte, jobName string) (domain.Job, error) {
	var def domain.Pipeline
	if err := json.Unmarshal(definition, &def); err != nil {
		return domain.Job{}, fmt.Errorf("scheduler: decode pipeline: %w", err)
	}
	jobDef, ok := findJob(def.Jobs, jobName)
	if !ok {
		return domain.Job{}, fmt.Errorf("scheduler: job %q not in pipeline definition", jobName)
	}
	return jobDef, nil
}

// effectiveNotificationAtIndex mirrors the run-create precedence:
// the pipeline's own Notifications list wins whenever it was
// declared (non-nil, even empty); the project's list only acts as
// a fallback when the pipeline never mentioned `notifications:`.
// Used by both the scheduler's trigger check and BuildAssignment
// so the dispatch path talks to exactly the same spec the run
// creator persisted.
func effectiveNotificationAtIndex(pipelineNotifs []domain.Notification, projectNotifsRaw []byte, idx int) (domain.Notification, bool) {
	source := pipelineNotifs
	if source == nil && len(projectNotifsRaw) > 0 {
		var projectNs []domain.Notification
		if err := json.Unmarshal(projectNotifsRaw, &projectNs); err == nil {
			source = projectNs
		}
	}
	if idx < 0 || idx >= len(source) {
		return domain.Notification{}, false
	}
	return source[idx], true
}

// resolveNotificationSpec is a thin wrapper over
// effectiveNotificationAtIndex that works off RunForDispatch —
// decodes the pipeline definition once, then defers to the
// shared precedence helper. Exposed so the scheduler's dispatch
// loop reads at the right abstraction ("resolve by run") without
// copy-pasting the Definition decode.
func resolveNotificationSpec(run store.RunForDispatch, idx int) (domain.Notification, bool) {
	var def struct {
		Notifications []domain.Notification `json:"Notifications"`
	}
	if err := json.Unmarshal(run.Definition, &def); err != nil {
		return domain.Notification{}, false
	}
	return effectiveNotificationAtIndex(def.Notifications, run.ProjectNotifications, idx)
}

// concurrencyFromDefinition pulls the pipeline-level concurrency
// setting from the JSONB snapshot. Empty / malformed / unknown
// values fall back to "" (parallel) so a bad definition never
// makes the scheduler wait forever — it's safer to race than
// deadlock.
func concurrencyFromDefinition(definition []byte) (string, error) {
	var def struct {
		Concurrency string `json:"Concurrency"`
	}
	if err := json.Unmarshal(definition, &def); err != nil {
		return "", fmt.Errorf("scheduler: decode pipeline: %w", err)
	}
	return def.Concurrency, nil
}

func findJob(jobs []domain.Job, name string) (domain.Job, bool) {
	for _, j := range jobs {
		if j.Name == name {
			return j, true
		}
	}
	return domain.Job{}, false
}

// materialCheckouts emits the gRPC MaterialCheckout entries the agent
// needs to clone each git material. When cloneTokens carries a token
// for the material id, the token is embedded in the URL as
// `https://x-access-token:TOKEN@host/...` so plain `git clone` picks
// it up without a credential helper, and the token is returned in the
// masks slice so the caller can append it to LogMasks. Non-https URLs
// are passed through untouched — SSH URLs need an in-pod SSH key, not
// a bearer.
func materialCheckouts(
	materials []store.Material,
	revs map[string]revisionSnapshot,
	cloneTokens map[string]string,
) ([]*gocdnextv1.MaterialCheckout, []string) {
	out := make([]*gocdnextv1.MaterialCheckout, 0, len(materials))
	var masks []string
	for _, m := range materials {
		if m.Type != string(domain.MaterialGit) {
			// Non-git materials don't need agent-side checkout (upstream/cron
			// are pure triggers; manual has no source code).
			continue
		}
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			continue
		}
		rev := revs[m.ID.String()]
		// Restore a clonable scheme on URLs that were canonicalised
		// at apply time. The store layer matches scm_sources rows by
		// the canonical scheme-less form (`github.com/owner/repo`);
		// `git clone` can't speak that, so HTTPCloneURL hands it the
		// HTTPS variant. URLs that already carry a scheme (or are
		// SSH shorthand) pass through.
		url := domain.HTTPCloneURL(cfg.URL)
		if tok := cloneTokens[m.ID.String()]; tok != "" {
			if rewritten, ok := injectBearerInHTTPSURL(url, tok); ok {
				url = rewritten
				masks = append(masks, tok)
			}
		}
		out = append(out, &gocdnextv1.MaterialCheckout{
			MaterialId: m.ID.String(),
			Url:        url,
			Revision:   rev.Revision,
			Branch:     firstNonEmpty(rev.Branch, cfg.Branch),
			TargetDir:  targetDirFor(m.ID),
			SecretRef:  cfg.SecretRef,
		})
	}
	return out, masks
}

// injectBearerInHTTPSURL rewrites `https://host/...` into
// `https://x-access-token:TOKEN@host/...` so plain git picks the
// credential up without a helper. Returns (original, false) for any
// shape that isn't a plain `https://` URL: SSH, ssh://, scheme-less
// canonical form, or already-embedded credentials all fall through
// to the unauthenticated clone path the operator can debug from logs.
func injectBearerInHTTPSURL(raw, token string) (string, bool) {
	const prefix = "https://"
	if !strings.HasPrefix(raw, prefix) || token == "" {
		return raw, false
	}
	rest := raw[len(prefix):]
	if strings.Contains(rest, "@") {
		// Pre-existing user:pass — leave it to the operator's intent.
		return raw, false
	}
	return prefix + "x-access-token:" + token + "@" + rest, true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func targetDirFor(id uuid.UUID) string {
	// Deterministic + short; agents create this dir under the workspace.
	return "src/" + id.String()[:8]
}

// dedupeArtifactPaths cleans the (required, optional) pair the
// agent receives so neither list contains canonically-identical
// duplicates AND optional never overlaps required. Defensive
// duplicate of the parser's apply-time dedupe (parse.go), applied
// here at dispatch so pipelines whose definition was persisted
// BEFORE the parser fix shipped still get a clean assignment.
//
// First-occurrence shape wins (operator's typing round-trips to
// the agent's tar entry name). Required wins over optional on
// cross-list collisions — the existing semantic.
//
// Uses store.NormalizeArtifactPath as the canonical form so the
// dedupe key here matches the one the storage layer's partial
// unique index enforces — drift between the two would let an
// "agent-deduped" assignment still trip the index. The package
// already imports store for other reasons, so reusing the helper
// has no dep cost.
func dedupeArtifactPaths(required, optional []string) (req, opt []string) {
	canonReq := make(map[string]struct{}, len(required))
	req = make([]string, 0, len(required))
	for _, p := range required {
		canon := store.NormalizeArtifactPath(p)
		if _, dup := canonReq[canon]; dup {
			continue
		}
		canonReq[canon] = struct{}{}
		req = append(req, p)
	}
	canonOpt := make(map[string]struct{}, len(optional))
	opt = make([]string, 0, len(optional))
	for _, p := range optional {
		canon := store.NormalizeArtifactPath(p)
		if _, dup := canonReq[canon]; dup {
			continue
		}
		if _, dup := canonOpt[canon]; dup {
			continue
		}
		canonOpt[canon] = struct{}{}
		opt = append(opt, p)
	}
	return req, opt
}

// coverageSpec lowers the parsed coverage_report into the proto
// spec. Nil-safe: most jobs don't declare coverage and the agent
// treats a nil spec as "nothing to scan".
func coverageSpec(cr *domain.CoverageReportSpec) *gocdnextv1.CoverageReportSpec {
	if cr == nil {
		return nil
	}
	return &gocdnextv1.CoverageReportSpec{
		Path:      cr.Path,
		Format:    cr.Format,
		FailUnder: cr.FailUnder,
	}
}
