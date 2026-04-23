// Package domain contains the core types that describe pipelines, materials,
// runs and everything in between. This is the canonical in-memory representation
// produced by the YAML parser and consumed by the scheduler.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// GitFingerprint returns the canonical fingerprint for a git material. Both
// the YAML parser and the webhook handler must compute identical values for
// (url, branch) so a push matches the material row stored at apply time.
// Normalization: trim whitespace, strip trailing slash, drop ".git" suffix,
// lowercase the host portion (path stays case-sensitive).
func GitFingerprint(cloneURL, branch string) string {
	u := normalizeGitURL(cloneURL)
	h := sha256.Sum256([]byte(u + "\x00" + branch))
	return hex.EncodeToString(h[:])
}

// UpstreamFingerprint is the canonical fingerprint for upstream-material
// triggers (one pipeline depending on another pipeline's stage success).
func UpstreamFingerprint(pipeline, stage string) string {
	h := sha256.Sum256([]byte("upstream\x00" + pipeline + "\x00" + stage))
	return hex.EncodeToString(h[:])
}

// CronFingerprint is the canonical fingerprint for a cron material.
func CronFingerprint(expression string) string {
	h := sha256.Sum256([]byte("cron\x00" + expression))
	return hex.EncodeToString(h[:])
}

// ManualFingerprint is the canonical fingerprint for a manual material.
// Manual materials don't carry configuration — the constant is fine.
func ManualFingerprint() string {
	h := sha256.Sum256([]byte("manual"))
	return hex.EncodeToString(h[:])
}

// NormalizeGitURL exposes the same normalization used internally by the
// fingerprint functions. Useful for matching repo URLs across sources
// (webhook payload vs scm_sources row vs material.git.url) without having
// to recompute a fingerprint just for comparison.
func NormalizeGitURL(raw string) string {
	return normalizeGitURL(raw)
}

func normalizeGitURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.Index(s, "://"); i != -1 {
		rest := s[i+3:]
		if j := strings.Index(rest, "/"); j != -1 {
			host := strings.ToLower(rest[:j])
			s = s[:i+3] + host + rest[j:]
		} else {
			s = s[:i+3] + strings.ToLower(rest)
		}
	}
	return s
}

// Concurrency values for the pipeline-level concurrency: knob.
//   - "parallel" (default): unlimited concurrent runs. Two pushes
//     to main produce two independent runs that race to finish.
//   - "serial": at most one run executes at a time. Additional
//     runs stay queued until the current one reaches a terminal
//     status, then the next queued one dispatches.
// Empty string parses as parallel so existing pipelines that
// never declared the field behave exactly as they did before.
const (
	ConcurrencyParallel = "parallel"
	ConcurrencySerial   = "serial"
)

type Pipeline struct {
	ID          string
	ProjectID   string
	Name        string
	Materials   []Material
	Stages      []string
	Jobs        []Job
	Variables   map[string]string
	Template    string
	// Concurrency controls whether multiple runs of this pipeline
	// can execute side by side. See the constants above.
	Concurrency string
	// TriggerEvents configures which SCM events fire the pipeline's
	// implicit project material (see injectImplicitProjectMaterial
	// in api/projects). Sourced from YAML's top-level `on:` field;
	// empty means "push only". Ignored for pipelines that declare
	// an explicit git material — those keep full control via the
	// material's own events list.
	TriggerEvents []string
	// Services are long-running sidecar containers (postgres, redis,
	// localstack, …) that every job in the pipeline can reach by
	// hostname. The agent brings them up on a job-scoped docker
	// network before tasks run, tears them down when the job
	// finishes (success, failure, or cancel). See runner.Execute.
	Services []Service
}

// Service describes a sidecar container that accompanies every
// job in the pipeline. Jobs reach it by `name` (used as the docker
// network alias). `command` overrides the image's entrypoint/cmd
// for cases like `postgres -c fsync=off`. Env is passed through to
// the service container as-is.
type Service struct {
	Name    string
	Image   string
	Env     map[string]string
	Command []string
}

type MaterialType string

const (
	MaterialGit      MaterialType = "git"
	MaterialUpstream MaterialType = "upstream"
	MaterialCron     MaterialType = "cron"
	MaterialManual   MaterialType = "manual"
)

type Material struct {
	ID          string       `json:"id,omitempty"`
	Type        MaterialType `json:"type"`
	Fingerprint string       `json:"fingerprint"`
	AutoUpdate  bool         `json:"auto_update"`
	// Implicit marks materials the apply layer synthesized (today:
	// the project-repo git material inferred from scm_source). The
	// runtime treats them like any other material, but the YAML
	// emitter hides them so the "yaml" tab mirrors what the operator
	// actually wrote instead of the stored+synthesized form.
	Implicit bool `json:"implicit,omitempty"`

	Git      *GitMaterial      `json:"git,omitempty"`
	Upstream *UpstreamMaterial `json:"upstream,omitempty"`
	Cron     *CronMaterial     `json:"cron,omitempty"`
}

// Material-config JSON tags match the YAML ones so `materials.config` in the
// DB stays human-readable (e.g. config->>'url') and queries/UI can inspect
// it without knowing Go's CamelCase field names.
type GitMaterial struct {
	URL                 string   `json:"url"`
	Branch              string   `json:"branch,omitempty"`
	Events              []string `json:"events,omitempty"`
	AutoRegisterWebhook bool     `json:"auto_register_webhook,omitempty"`
	SecretRef           string   `json:"secret_ref,omitempty"`
}

type UpstreamMaterial struct {
	Pipeline string `json:"pipeline"`
	Stage    string `json:"stage"`
	Status   string `json:"status,omitempty"`
}

type CronMaterial struct {
	Expression string `json:"expression"`
}

type Job struct {
	Name      string
	Stage     string
	Image     string
	Needs     []string
	Tasks     []Task
	Settings  map[string]string
	Variables map[string]string
	Matrix    map[string][]string
	Rules     []Rule
	// Secrets are project-secret names whose values should be injected into
	// the job env at dispatch time. The runner also masks these values in
	// log lines. Kept as a list of names (not values) so the YAML and the
	// stored pipeline definition never carry plaintext.
	Secrets []string
	// Tags restrict which agents may run this job. A job dispatches only to
	// a session whose Tags is a superset of this list. Empty = any agent.
	Tags []string
	// ArtifactPaths are the file/directory paths the runner should tar+gz
	// and upload after the job succeeds. YAML source: `artifacts.paths:`.
	// Failure to upload any of these fails the job — the YAML declared
	// these as part of the build's output contract, so a missing file
	// means the build didn't deliver what it promised.
	ArtifactPaths []string
	// OptionalArtifactPaths are best-effort uploads (`artifacts.optional:`
	// in YAML). The runner attempts each but logs and continues on
	// failure — useful for coverage reports, screenshots, or debug logs
	// that should ship when possible but never gate the build.
	OptionalArtifactPaths []string
	// ArtifactDeps declare artefacts this job consumes from earlier jobs
	// in the same run. Agent downloads each before tasks start; a
	// missing/not-ready dep fails the job cleanly.
	ArtifactDeps []ArtifactDep
	// Docker asks the agent to expose a Docker API inside the job
	// (via docker.sock mount or DinD sidecar, depending on engine)
	// so scripts can spawn containers themselves — testcontainers,
	// docker compose, buildx. Engines that can't honour this fail
	// the job instead of silently running without it.
	Docker bool
}

// ArtifactDep is one entry in `needs_artifacts`. FromJob is the name
// of the producing job. FromPipeline, when set, switches resolution
// from the current run to the *upstream* run that triggered this one
// (fanout case) and names the pipeline we expect that run to belong
// to. Empty FromPipeline means intra-run. Paths (optional) filters
// which of that job's artifacts to pull — empty means all. Dest
// defaults to "./" (workspace root).
type ArtifactDep struct {
	FromJob      string
	FromPipeline string
	Paths        []string
	Dest         string
}

type Task struct {
	Script string
	Plugin *PluginStep
}

type PluginStep struct {
	Image    string
	Settings map[string]string
}

type Rule struct {
	IfExpr  string
	When    string
	Changes []string
}

type RunStatus string

const (
	StatusQueued   RunStatus = "queued"
	StatusRunning  RunStatus = "running"
	StatusSuccess  RunStatus = "success"
	StatusFailed   RunStatus = "failed"
	StatusCanceled RunStatus = "canceled"
	StatusSkipped  RunStatus = "skipped"
	StatusWaiting  RunStatus = "waiting"
)

type BuildCause string

const (
	CauseWebhook  BuildCause = "webhook"
	CauseUpstream BuildCause = "upstream"
	CauseManual   BuildCause = "manual"
	CauseSchedule BuildCause = "schedule"
	CausePoll     BuildCause = "poll"
)

type Revision struct {
	MaterialID string
	Revision   string
	Branch     string
	Message    string
	Author     string
	Timestamp  time.Time
}

type Run struct {
	ID          string
	PipelineID  string
	Counter     int64
	Cause       BuildCause
	Revisions   []Revision
	Status      RunStatus
	StartedAt   *time.Time
	FinishedAt  *time.Time
	TriggeredBy string
}
