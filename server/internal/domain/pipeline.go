// Package domain contains the core types that describe pipelines, materials,
// runs and everything in between. This is the canonical in-memory representation
// produced by the YAML parser and consumed by the scheduler.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
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
	// Notifications declare post-run side effects. Each entry fires
	// when the run reaches terminal and its `On` predicate matches
	// (failure | success | always | canceled). Plugins are resolved
	// the same way jobs resolve `uses:` — a notifier plugin image
	// (slack/discord/email/…) is invoked with `With` + `Secrets`.
	Notifications []Notification
}

// NotificationTrigger enumerates the supported `on:` values. Keep
// in sync with parser.notificationTriggers and the dispatcher.
type NotificationTrigger string

const (
	NotifyOnFailure  NotificationTrigger = "failure"
	NotifyOnSuccess  NotificationTrigger = "success"
	NotifyOnAlways   NotificationTrigger = "always"
	NotifyOnCanceled NotificationTrigger = "canceled"
)

// NotificationStageName is the reserved stage name used when the
// store synthesizes a trailing stage to hold notification jobs.
// The parser rejects user pipelines that try to declare a stage
// with this name so a user-written stage doesn't collide with
// the synth one's ordinal slot.
const NotificationStageName = "_notifications"

// NotificationJobName builds the synthetic job_run name for a
// notification at index `idx`. Encoding the index is enough —
// the trigger is read back from pipeline.Notifications[idx] at
// dispatch time, so we don't have to pack trigger into the name.
func NotificationJobName(idx int) string {
	return fmt.Sprintf("_notify_%d", idx)
}

// IsNotificationJobName reports whether a job_run name was
// produced by NotificationJobName. Used by the scheduler +
// assignment paths to branch into the synth lookup (pull spec
// from pipeline.Notifications instead of pipeline.Jobs).
func IsNotificationJobName(name string) bool {
	return strings.HasPrefix(name, "_notify_")
}

// NotificationIndexFromName recovers the index out of a synth
// job_run name. Returns -1 + ok=false when the name wasn't
// produced by NotificationJobName.
func NotificationIndexFromName(name string) (int, bool) {
	if !IsNotificationJobName(name) {
		return -1, false
	}
	rest := strings.TrimPrefix(name, "_notify_")
	i, err := strconv.Atoi(rest)
	if err != nil || i < 0 {
		return -1, false
	}
	return i, true
}

// ResolvePluginRef translates a `uses:` reference into the
// Docker image spec the runner feeds to `docker run`. Accepted
// shapes mirror GitHub Actions:
//
//	owner/name                     → "owner/name"
//	owner/name@v1                  → "owner/name:v1"
//	owner/name@sha256:<hex>        → digest pin, passed through
//	registry.io/owner/name@v1      → "registry.io/owner/name:v1"
//
// Lives in domain (not parser) so the store's run-materialization
// path can resolve notification refs without importing the parser
// layer — the boundary stays "parser makes domain, store persists
// domain". Tag validation is intentionally lax; Docker rejects
// garbage tags at pull time with a clear error.
func ResolvePluginRef(uses string) (string, error) {
	uses = strings.TrimSpace(uses)
	if uses == "" {
		return "", fmt.Errorf("`uses:` is empty")
	}
	if strings.ContainsAny(uses, " \t\n") {
		return "", fmt.Errorf("`uses:` contains whitespace: %q", uses)
	}
	at := strings.Index(uses, "@")
	if at < 0 {
		return uses, nil
	}
	base, suffix := uses[:at], uses[at+1:]
	if base == "" {
		return "", fmt.Errorf("`uses:` missing image before `@`: %q", uses)
	}
	if suffix == "" {
		return "", fmt.Errorf("`uses:` missing version after `@`: %q", uses)
	}
	if strings.HasPrefix(suffix, "sha256:") {
		return uses, nil
	}
	return base + ":" + suffix, nil
}

// Notification is one post-run side effect. Shape mirrors the
// plugin-style `uses:` + `with:` + `secrets:` contract so the
// dispatcher can reuse the existing agent/runner path instead
// of inventing a second invocation model.
type Notification struct {
	On      NotificationTrigger
	Uses    string
	With    map[string]string
	Secrets []string
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
	// PollInterval triggers a server-side check of the branch
	// HEAD every N duration. Zero (default) disables polling —
	// the material only advances on webhook or explicit sync.
	// Parser clamps to [1m, 24h] so the tick worker isn't
	// hammered and very-stale polls don't masquerade as webhook
	// replacements. Stored as int64 nanoseconds in the JSONB
	// config column; the poll worker parses back via the normal
	// time.Duration path.
	PollInterval time.Duration `json:"poll_interval,omitempty"`
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
	// Caches the agent should fetch before tasks run and re-upload
	// after a successful run. Each entry is keyed by name with a
	// list of directories to tar up. Same key across runs hits the
	// same blob (project-scoped) so a pnpm store or a go build
	// cache carries over without round-tripping a full artifact.
	// Cache misses are silent — the job runs without a pre-
	// populated directory and the post-run upload seeds it.
	Cache []CacheSpec
	// Approval, when non-nil, marks this job as a manual gate:
	// scheduler never dispatches it, it parks in `awaiting_approval`
	// until a user with permission calls the approve endpoint.
	// Reject transitions the job to `failed` with reason
	// "rejected by <user>". Jobs downstream via `needs:` stay
	// queued until approval fires. Approval jobs MUST NOT declare
	// `image:`, `tasks:`, `uses:`, or `artifacts:` — their only
	// side effect is the state transition, not execution.
	Approval *ApprovalSpec
	// TestReports is the glob list the agent scans after tasks
	// complete, parses as JUnit XML, and sends back as a
	// TestResultBatch so the server can render a Tests tab.
	TestReports []string
}

// ApprovalSpec is the shape of a manual-gate job. `Approvers`
// is an allow-list of usernames that can approve; empty means
// "any authenticated user" (bad default for prod, but fine for
// dev/demo pipelines). `Description` is surfaced in the UI as
// the prompt the approver sees before clicking Approve/Reject.
type ApprovalSpec struct {
	Approvers   []string
	// ApproverGroups are group names (not ids) whose members can
	// approve the gate in addition to the individual Approvers
	// list. Names (not ids) so renames propagate cleanly without
	// touching persisted gate rows.
	ApproverGroups []string
	// Required is the quorum — minimum distinct approvals before
	// the gate transitions to success. 1 (default) matches the
	// original "any single approver" semantics. A single reject
	// from any allowed user still fails the gate immediately,
	// regardless of how high Required is set.
	Required    int
	Description string
}

// CacheSpec pairs a cache key with the directories the agent
// should persist between runs. `Key` is the string identifier
// (project-scoped); `Paths` are workspace-relative directories
// tar'd up into the cache blob after the job succeeds and
// extracted into the workspace before tasks start on a later
// run with the same key.
type CacheSpec struct {
	Key   string
	Paths []string
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
