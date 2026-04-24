// Package parser reads `.gocdnext.yaml` files and produces domain.Pipeline.
//
// The YAML schema borrows from three tools:
//   - GoCD: stages/jobs/tasks, upstream materials, templates
//   - GitLab CI: stages list, rules, needs, extends/include, parallel matrix
//   - Woodpecker: plugin = container + settings (PLUGIN_* env vars)
package parser

// File is the top-level structure of a YAML file inside `.gocdnext/`.
// Each file defines exactly one pipeline. The folder may contain many files.
//
// Pipeline name resolution order:
//  1. `name:` field (preferred, explicit)
//  2. filename without extension (fallback)
type File struct {
	Version   string            `yaml:"version,omitempty"` // reserved for future
	Name      string            `yaml:"name,omitempty"`    // pipeline name; defaults to filename
	Include   []Include         `yaml:"include,omitempty"`
	Materials []MaterialSpec    `yaml:"materials,omitempty"`
	Stages    []string          `yaml:"stages"`
	Variables map[string]string `yaml:"variables,omitempty"`
	Template  string            `yaml:"template,omitempty"`
	Jobs      map[string]JobDef `yaml:"jobs"`
	// Concurrency: "" / "parallel" (default, unlimited concurrent
	// runs) or "serial" (one run at a time — subsequent triggers
	// queue behind the running one).
	Concurrency string `yaml:"concurrency,omitempty"`
	// When configures the pipeline's implicit project material (the
	// git source bound via scm_source). Mirrors the job-level
	// WhenDef shape. Today only `event:` is wired — drives which
	// SCM events create runs. `branch:` / `status:` are reserved
	// for future use at this level.
	When *WhenDef `yaml:"when,omitempty"`
	// Services are sidecar containers the agent runs alongside
	// every job in this pipeline — postgres, redis, localstack,
	// etc. Jobs reach them by `name` over a shared docker network.
	// See domain.Service for runtime semantics.
	Services []ServiceSpec `yaml:"services,omitempty"`
	// Notifications are post-run side effects. Each entry picks a
	// trigger (on: failure|success|always|canceled), a notifier
	// plugin (uses: …), and its inputs. Dispatched when the run
	// reaches terminal.
	Notifications []NotificationSpec `yaml:"notifications,omitempty"`
}

// NotificationSpec is the YAML shape for one post-run notification.
// It deliberately mirrors the plugin-job contract (`uses:` + `with:`
// + `secrets:`) so the dispatcher reuses the existing plugin path
// instead of inventing a second invocation model.
type NotificationSpec struct {
	On      string            `yaml:"on"`
	Uses    string            `yaml:"uses"`
	With    map[string]string `yaml:"with,omitempty"`
	Secrets []string          `yaml:"secrets,omitempty"`
}

// ServiceSpec is the YAML shape for a pipeline-level sidecar
// container. Everything is optional except `image` — `name`
// defaults to the image's short name when omitted so simple
// single-service pipelines stay concise.
type ServiceSpec struct {
	Name    string            `yaml:"name,omitempty"`
	Image   string            `yaml:"image"`
	Env     map[string]string `yaml:"env,omitempty"`
	Command []string          `yaml:"command,omitempty"`
}

type Include struct {
	Local    string `yaml:"local,omitempty"`
	Remote   string `yaml:"remote,omitempty"`
	Template string `yaml:"template,omitempty"`
}

// MaterialSpec — one of the fields must be set.
type MaterialSpec struct {
	Git      *GitSpec      `yaml:"git,omitempty"`
	Upstream *UpstreamSpec `yaml:"upstream,omitempty"`
	Cron     *CronSpec     `yaml:"cron,omitempty"`
	Manual   bool          `yaml:"manual,omitempty"`
}

type GitSpec struct {
	URL                 string   `yaml:"url"`
	Branch              string   `yaml:"branch,omitempty"`
	On                  []string `yaml:"on,omitempty"` // push, pull_request
	AutoRegisterWebhook bool     `yaml:"auto_register_webhook,omitempty"`
	SecretRef           string   `yaml:"secret_ref,omitempty"`
	// PollInterval is a Go duration string ("5m", "30m", "1h").
	// When set, the server checks branch HEAD every N and creates
	// a modification when it advances — covers firewall-bound
	// repos that can't reach us via webhook. Zero/empty disables.
	PollInterval string `yaml:"poll_interval,omitempty"`
}

type UpstreamSpec struct {
	Pipeline string `yaml:"pipeline"`
	Stage    string `yaml:"stage"`
	Status   string `yaml:"status,omitempty"`
}

type CronSpec struct {
	Expression string `yaml:"expression"`
}

// JobDef — definition of one job. Supports `extends` via YAML anchors.
type JobDef struct {
	Stage     string            `yaml:"stage"`
	Image     string            `yaml:"image,omitempty"`
	Extends   string            `yaml:"extends,omitempty"`
	Needs     []string          `yaml:"needs,omitempty"`
	Script    []string          `yaml:"script,omitempty"`
	Settings  map[string]string `yaml:"settings,omitempty"` // plugin step (legacy)
	// Uses + With are the GH-Actions-flavoured sugar for a plugin
	// job: `uses:` names a container image that ships its own
	// entrypoint, `with:` is the settings map the agent translates
	// to PLUGIN_* env vars before `docker run`ing the image. No
	// `script:` needed — the plugin's entrypoint IS the logic.
	// Mutually exclusive with `image` + `settings` on the same job;
	// parser rejects the combination.
	Uses string            `yaml:"uses,omitempty"`
	With map[string]string `yaml:"with,omitempty"`
	Variables map[string]string `yaml:"variables,omitempty"`
	// Cache entries let this job reuse tar'd directories across
	// runs, keyed by name. The agent fetches each cache BEFORE
	// tasks (cache miss = silent skip) and uploads it after
	// success. Scope is per-project: two pipelines using the
	// same key intentionally share the blob. Dead schema before
	// — wired end-to-end starting with commit that lands this
	// comment.
	Cache          []CacheSpec         `yaml:"cache,omitempty"`
	Artifacts      *Artifacts          `yaml:"artifacts,omitempty"`
	NeedsArtifacts []NeedsArtifactDef  `yaml:"needs_artifacts,omitempty"`
	// TestReports is a list of glob patterns the agent scans
	// after tasks complete. Each match is parsed as JUnit/xUnit
	// XML and the per-case results ship back as a TestResultBatch
	// alongside the JobResult. Empty list = no test reporting.
	TestReports    []string            `yaml:"test_reports,omitempty"`
	Parallel       *Parallel           `yaml:"parallel,omitempty"`
	Rules          []RuleDef           `yaml:"rules,omitempty"`
	When           *WhenDef            `yaml:"when,omitempty"`
	Timeout        string              `yaml:"timeout,omitempty"`
	Retry          int                 `yaml:"retry,omitempty"`
	Secrets        []string            `yaml:"secrets,omitempty"`
	Tags           []string            `yaml:"tags,omitempty"`
	// Docker = true asks the agent for docker API access inside the
	// job (socket mount / DinD sidecar). Pair with `image:` to spawn
	// sibling containers for testcontainers / docker compose.
	Docker bool `yaml:"docker,omitempty"`
	// Approval flags this job as a manual gate. When set the job
	// never dispatches to an agent — it parks in awaiting_approval
	// and transitions via the approve/reject HTTP endpoints. No
	// `script`, `uses`, `image`, or `artifacts` allowed alongside
	// it; parser rejects the combination.
	Approval *ApprovalDef `yaml:"approval,omitempty"`
}

// ApprovalDef is the YAML shape of a manual approval gate. Kept
// explicit (`approval:` sub-object) rather than a top-level
// `type: approval` field because the other knobs (approvers,
// description) only make sense in the approval context — a
// sub-object keeps unrelated schema from bleeding across the job.
type ApprovalDef struct {
	Approvers   []string `yaml:"approvers,omitempty"`
	Description string   `yaml:"description,omitempty"`
}

// NeedsArtifactDef is one entry of a job's `needs_artifacts:` list. It
// addresses an upstream job's artefacts. Scope is intra-run by
// default; setting `from_pipeline` switches to cross-run (fanout):
// the scheduler resolves the run that triggered *this* run (via
// `upstream:` material) and pulls from there instead. Useful for
// pipelines that declare `upstream:` and want bits from the parent
// run.
type NeedsArtifactDef struct {
	FromJob      string   `yaml:"from_job"`
	FromPipeline string   `yaml:"from_pipeline,omitempty"` // "" = same run; set = upstream run triggered by this pipeline
	Paths        []string `yaml:"paths,omitempty"`          // subset filter; empty = all
	Dest         string   `yaml:"dest,omitempty"`           // default "./"
}

// CacheSpec is one entry in a job's `cache:` list. `key` identifies
// the cache blob across runs within a project; `paths` are the
// directories the agent tars up after the job succeeds and untars
// before the next job with the same key. Empty `paths` is a config
// error (the parser rejects it) — a keyed cache with nothing to
// save is just noise.
type CacheSpec struct {
	Key   string   `yaml:"key"`
	Paths []string `yaml:"paths"`
}

type Artifacts struct {
	Paths []string `yaml:"paths"`
	// Optional is a best-effort list — upload failures log but don't
	// fail the job. Use for coverage, screenshots, debug logs. Paths
	// listed in both `paths` and `optional` are treated as required
	// (required wins; see parser).
	Optional []string `yaml:"optional,omitempty"`
	ExpireIn string   `yaml:"expire_in,omitempty"`
	When     string   `yaml:"when,omitempty"` // on_success|on_failure|always
}

type Parallel struct {
	Matrix []map[string][]string `yaml:"matrix,omitempty"`
	Count  int                   `yaml:"count,omitempty"`
}

type RuleDef struct {
	If      string   `yaml:"if,omitempty"`
	Changes []string `yaml:"changes,omitempty"`
	Exists  []string `yaml:"exists,omitempty"`
	When    string   `yaml:"when,omitempty"` // always|manual|never|on_success
}

type WhenDef struct {
	Status []string `yaml:"status,omitempty"` // success|failure|always
	Branch []string `yaml:"branch,omitempty"`
	Event  []string `yaml:"event,omitempty"`
}
