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
// Normalization collapses SSH (git@host:owner/repo) and HTTPS
// (https://host/owner/repo) into the same canonical host/path form so
// they hash identically — GitHub webhook payloads always deliver the
// HTTPS clone_url even when the project's scm_source was registered
// with the SSH form, and a fingerprint mismatch silently drops the run.
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

// HTTPCloneURL turns a canonical scheme-less URL (NormalizeGitURL
// output, e.g. `github.com/owner/repo`) back into a clonable HTTPS
// URL. Used at dispatch + material-write time so the agent's
// `git clone` always sees a fully-qualified URL even though the
// canonical form is what the store layer matches on. URLs that
// already carry a scheme (https://, http://, ssh://) or the SSH
// shorthand (`git@host:`) pass through unchanged. Empty input
// returns empty so callers don't have to special-case it.
func HTTPCloneURL(canonical string) string {
	s := strings.TrimSpace(canonical)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") || strings.HasPrefix(s, "git@") {
		return s
	}
	return "https://" + s
}

// normalizeGitURL canonicalises a git URL into a `host/owner/repo`
// scheme-less form so SSH and HTTPS spellings of the same repo hash
// identically. Recognised inputs:
//
//	https://github.com/owner/repo[.git]    →  github.com/owner/repo
//	ssh://git@github.com/owner/repo[.git]  →  github.com/owner/repo
//	git@github.com:owner/repo[.git]        →  github.com/owner/repo
//
// Anything that does not match a recognised shape (e.g. a bare local
// path) is returned lowercased on the host portion when possible and
// otherwise untouched, so legacy callers and tests that fed odd
// fixtures still get a stable string.
func normalizeGitURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")

	// SSH shorthand: git@host:owner/repo. The `:` separates host
	// from path (unlike URL form which always has `/`). We strip
	// the leading `git@`, lowercase the host, and rewrite the
	// colon as a slash so it shares the canonical form with the
	// URL paths below.
	if strings.HasPrefix(s, "git@") {
		after := strings.TrimPrefix(s, "git@")
		if i := strings.Index(after, ":"); i > 0 {
			host := strings.ToLower(after[:i])
			path := strings.TrimLeft(after[i+1:], "/")
			return host + "/" + path
		}
		return strings.ToLower(s)
	}

	// scheme://[user@]host[:port]/path — drop the scheme and any
	// embedded credentials, lowercase the host, keep the path
	// case-sensitive (forge paths often are).
	if i := strings.Index(s, "://"); i != -1 {
		rest := s[i+3:]
		if at := strings.Index(rest, "@"); at != -1 {
			// Strip credentials before the host (e.g. ssh://git@…).
			rest = rest[at+1:]
		}
		if j := strings.Index(rest, "/"); j != -1 {
			host := strings.ToLower(rest[:j])
			return host + rest[j:]
		}
		return strings.ToLower(rest)
	}

	return s
}

// Concurrency values for the pipeline-level concurrency: knob.
//   - "parallel" (default): unlimited concurrent runs. Two pushes
//     to main produce two independent runs that race to finish.
//   - "serial": at most one run executes at a time. Additional
//     runs stay queued until the current one reaches a terminal
//     status, then the next queued one dispatches.
//
// Empty string parses as parallel so existing pipelines that
// never declared the field behave exactly as they did before.
const (
	ConcurrencyParallel = "parallel"
	ConcurrencySerial   = "serial"
)

// Supersede values for the pipeline-level supersede: knob (issue #97).
// Latest-wins for approval-gated pipelines, scoped to a "lane":
//   - "off" (default; empty parses as off): no supersede — current behaviour.
//   - "branch": lane = (pipeline, ref) — latest ready revision wins WITHIN a
//     branch; feature branches are independent lanes.
//   - "pipeline": lane = (pipeline) — any newer revision wins regardless of ref
//     (single-target prod pipelines).
const (
	SupersedeOff      = "off"
	SupersedeBranch   = "branch"
	SupersedePipeline = "pipeline"
)

type Pipeline struct {
	ID        string
	ProjectID string
	Name      string
	Materials []Material
	Stages    []string
	Jobs      []Job
	Variables map[string]string
	Template  string
	// Concurrency controls whether multiple runs of this pipeline
	// can execute side by side. See the constants above.
	Concurrency string
	// Supersede controls latest-wins cancellation of older pending
	// approvals + a stale-deploy backstop for gated pipelines (#97).
	// See the Supersede* constants; empty == off.
	Supersede string
	// TriggerEvents configures which SCM events fire the pipeline's
	// implicit project material (see injectImplicitProjectMaterial
	// in api/projects). Sourced from YAML's top-level
	// `when.event:` field; empty means "push only". Ignored for
	// pipelines that declare an explicit git material — those keep
	// full control via the material's own events list. Accepted
	// values: "push", "pull_request", "tag" (since v0.10.0 — tag
	// pushes match URL-only, branch-agnostic).
	TriggerEvents []string
	// TriggerBranches whitelists branches that fire the pipeline's
	// implicit project material. Sourced from YAML's top-level
	// `when.branch:` list. Empty falls back to the scm_source's
	// default_branch (today's single-branch behaviour). When set,
	// the injection creates ONE implicit material per branch so each
	// branch's push fingerprint matches a distinct row — same
	// dispatch path as multi-explicit-material pipelines.
	TriggerBranches []string
	// TriggerPaths gates run creation on the changed-file set of the
	// triggering push/PR (YAML `when.paths:`). Doublestar globs,
	// repo-relative — the pipeline fires when at least one changed
	// file matches one glob. Empty = always fire. Unknown file set
	// (Bitbucket pushes, truncated payloads, files-API failure) =
	// fail open. Lowered into the implicit material's Paths by
	// configsync, same flow as TriggerEvents.
	TriggerPaths []string
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
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
	// Events enumerates the SCM events that fire this material.
	// Accepted values: "push" (branch push — default when omitted),
	// "pull_request" (PR open/sync/reopen, matched against the PR's
	// base ref), "tag" (any tag push for the repo, branch-agnostic —
	// the webhook handler matches by URL only and filters down to
	// materials whose Events contains "tag"). Empty defaults to
	// ["push"] in the parser.
	Events              []string `json:"events,omitempty"`
	AutoRegisterWebhook bool     `json:"auto_register_webhook,omitempty"`
	SecretRef           string   `json:"secret_ref,omitempty"`
	// Paths gates this material's run creation on the triggering
	// event's changed-file set (doublestar globs, repo-relative).
	// Populated from the pipeline's `when.paths:` by the implicit-
	// material injection. Empty = always fire; unknown file set =
	// fail open (see webhook/pathfilter.go).
	Paths []string `json:"paths,omitempty"`
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

// ResourceSpec is the per-job compute envelope. Strings carry the
// canonical Kubernetes format ("100m", "256Mi", "1.5", "2Gi"); empty
// means "not set" and falls back to the resolved profile defaults.
// Engines that don't honour resources (Shell) ignore the field; the
// scheduler still validates it against the profile cap so a YAML
// that breaches policy is caught at apply time regardless of where
// it would have ultimately landed.
type ResourceSpec struct {
	Requests ResourceQuantities
	Limits   ResourceQuantities
}

// ResourceQuantities mirrors corev1.ResourceList but with string
// values to keep the domain layer free of k8s imports.
type ResourceQuantities struct {
	CPU    string
	Memory string
}

// IsZero reports whether the spec carries no user-set values.
// Used to decide if profile defaults should fill in.
func (r ResourceSpec) IsZero() bool {
	return r.Requests == (ResourceQuantities{}) && r.Limits == (ResourceQuantities{})
}

type Job struct {
	Name  string
	Stage string
	Image string
	// Profile names a runner_profiles row resolved at apply time.
	// Empty = "any agent, any defaults" (legacy behaviour).
	Profile string
	// Cluster names a clusters-registry row (k8s deploy target).
	// Resolved + authorized at apply; its kubeconfig is injected as
	// PLUGIN_KUBECONFIG at dispatch. Empty = no managed cluster.
	// JSON field name "Cluster" is the jsonpath the delete-guard +
	// usage queries match (CountClusterUsage).
	Cluster string `json:",omitempty"`
	// Resources is the user-declared compute envelope. Profile
	// defaults fill the empty fields; profile.max caps non-empty
	// fields; values surviving validation flow into the runtime
	// (k8s Pod resources).
	Resources ResourceSpec
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
	// ArtifactsWhen gates artifact upload on the task outcome
	// (`artifacts.when:` in YAML): "on_success" (default — upload only
	// when every task succeeded), "on_failure" (upload only when a task
	// failed), or "always". Empty means on_success. This is what lets a
	// blocking scanner (exit-code 1 on a finding) still publish its SARIF
	// so the Security dashboard sees the very findings that failed the job.
	ArtifactsWhen string
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
	// CoverageReport, when non-nil, points the agent at one
	// coverage file to parse after tasks complete. Only the parsed
	// SUMMARY crosses the wire; the raw file stays in the
	// workspace. JSON tags keep the JSONB definition readable.
	CoverageReport *CoverageReportSpec `json:",omitempty"`
	// TestReports is the glob list the agent scans after tasks
	// complete, parses as JUnit XML, and sends back as a
	// TestResultBatch so the server can render a Tests tab.
	TestReports []string
	// Outputs declares the structured k/v this job promises to
	// produce, as a map from YAML alias → plugin env-var name read
	// from $GOCDNEXT_OUTPUT_FILE. Downstream jobs reference any
	// alias via `${{ needs.<this-job>.outputs.<alias> }}`, resolved
	// at dispatch against the persisted job_runs.outputs of the
	// upstream. Empty / nil = the job doesn't promise any outputs.
	// See issue #10.
	Outputs map[string]string
	// OutputMasks lists which aliases (keys in Outputs) are
	// opt-in masked via the object-form YAML
	// (`alias: {env: NAME, masked: true}`). When the scheduler
	// resolves `${{ needs.X.outputs.<masked-alias> }}` for a
	// downstream job, the resolved value is added to LogMasks
	// so it gets scrubbed in agent log streams. Defence-in-depth
	// on top of the heuristic 8+-char auto-mask, and an explicit
	// operator-visible contract for "this value IS sensitive".
	// Empty / nil = no masking opt-ins. See issue #22.
	OutputMasks map[string]bool

	// IDTokens declares per-job OIDC tokens (id_tokens: in YAML).
	// Map key = env var name the JWT is injected as at dispatch;
	// the spec carries the required audience list. The token VALUE
	// never lives here — it's minted fresh per dispatch and exists
	// only in the JobAssignment env + LogMasks. Aud is config, not
	// secret, so JSONB persistence of the definition is fine.
	//
	// omitempty is LOAD-BEARING: the scheduler's dispatch fast path
	// gates on bytes.Contains(definition, `"IDTokens"`) before
	// paying any JSON decode — without omitempty every persisted
	// job would carry `"IDTokens":null` and the gate would never
	// skip.
	IDTokens map[string]IDTokenSpec `json:",omitempty"`

	// Deploy, when non-nil, marks this (executable) job as a
	// deployment to a named environment — a tracking marker the
	// scheduler/result path turns into a deployment_revision. See
	// DeploySpec. omitempty: the dispatch fast-path and the common
	// non-deploy job never carry "Deploy":null.
	Deploy *DeploySpec `json:",omitempty"`
}

// IDTokenSpec is the audience contract for one job id_token.
type IDTokenSpec struct {
	Aud []string
}

// DeploySpec marks an executable job as a deployment to a named
// environment (#39). It is a TRACKING marker, NOT an executor: the
// job's plugin/script still performs the deploy. On dispatch the
// scheduler resolves Version (refs allowed; empty defaults to the
// commit short sha) and records a deployment_revision against the
// project's environment (lazy-created on first reference); on the
// job's terminal result the revision finalises to success/failed.
//
// Version is opaque to gocdnext — whatever string identifies "what
// was deployed" (image tag, chart version, git sha, app revision).
// It is metadata only: the plugin reads its OWN refs in `with:`, so
// rollback (re-running the deploy job of a past run) re-resolves the
// old version for free from that run's immutable outputs.
//
// omitempty keeps the JSONB definition free of "Deploy":null on the
// common (non-deploy) job, mirroring IDTokens.
type DeploySpec struct {
	// Environment is the deploy target name (production, staging,
	// …), unique per project. Lazy-created on first reference.
	Environment string
	// Version is the raw (possibly ref-bearing) string recorded as
	// the deployed version. Empty = default to commit short sha at
	// dispatch.
	Version string `json:",omitempty"`
}

// ApprovalSpec is the shape of a manual-gate job. `Approvers`
// is an allow-list of usernames that can approve; empty means
// "any authenticated user" (bad default for prod, but fine for
// dev/demo pipelines). `Description` is surfaced in the UI as
// the prompt the approver sees before clicking Approve/Reject.
type ApprovalSpec struct {
	Approvers []string
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
	Required int
	// QuorumByLabel maps a PR label name (lowercased) to a quorum
	// override that applies when the run was triggered by a PR
	// carrying that label. Nil/empty = no overrides; the gate uses
	// Required for every run.
	//
	// Resolution at run materialisation: the materialiser
	// intersects the PR's labels (cause_detail.pr_labels) with this
	// map's keys; when multiple match, the LARGEST override wins
	// (defensive — two labels both wanting more quorum shouldn't
	// cancel). For non-PR causes (push, tag, manual, upstream,
	// schedule, poll) the map is ignored and Required applies.
	//
	// The override is a snapshot at materialisation time. Relabel
	// the PR after the run is created → push a new head to
	// re-materialise; the existing gate keeps its frozen quorum.
	QuorumByLabel map[string]int
	Description   string
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

// CoverageReportSpec is the persisted shape of `coverage_report:`.
// Format is parse-time validated against the closed set the agent
// implements (go-cover | lcov | cobertura).
type CoverageReportSpec struct {
	Path   string `json:"path"`
	Format string `json:"format"`
	// FailUnder > 0 turns the report into a gate: the agent fails
	// the job when total coverage lands below this percentage.
	FailUnder float64 `json:"fail_under,omitempty"`
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
	CauseWebhook     BuildCause = "webhook"
	CausePullRequest BuildCause = "pull_request"
	CauseTag         BuildCause = "tag"
	CauseUpstream    BuildCause = "upstream"
	CauseManual      BuildCause = "manual"
	CauseSchedule    BuildCause = "schedule"
	CausePoll        BuildCause = "poll"
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
