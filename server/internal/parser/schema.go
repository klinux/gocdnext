// Package parser reads `.gocdnext.yaml` files and produces domain.Pipeline.
//
// The YAML schema borrows from three tools:
//   - GoCD: stages/jobs/tasks, upstream materials, templates
//   - GitLab CI: stages list, rules, needs, extends/include, parallel matrix
//   - Woodpecker: plugin = container + settings (PLUGIN_* env vars)
package parser

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

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
	Stage    string            `yaml:"stage"`
	Image    string            `yaml:"image,omitempty"`
	Extends  string            `yaml:"extends,omitempty"`
	Needs    []string          `yaml:"needs,omitempty"`
	Script   []string          `yaml:"script,omitempty"`
	Settings map[string]string `yaml:"settings,omitempty"` // plugin step (legacy)
	// Uses + With are the GH-Actions-flavoured sugar for a plugin
	// job: `uses:` names a container image that ships its own
	// entrypoint, `with:` is the settings map the agent translates
	// to PLUGIN_* env vars before `docker run`ing the image. No
	// `script:` needed — the plugin's entrypoint IS the logic.
	// Mutually exclusive with `image` + `settings` on the same job;
	// parser rejects the combination.
	Uses      string            `yaml:"uses,omitempty"`
	With      map[string]string `yaml:"with,omitempty"`
	Variables map[string]string `yaml:"variables,omitempty"`
	// Cache entries let this job reuse tar'd directories across
	// runs, keyed by name. The agent fetches each cache BEFORE
	// tasks (cache miss = silent skip) and uploads it after
	// success. Scope is per-project: two pipelines using the
	// same key intentionally share the blob. Dead schema before
	// — wired end-to-end starting with commit that lands this
	// comment.
	Cache          []CacheSpec        `yaml:"cache,omitempty"`
	Artifacts      *Artifacts         `yaml:"artifacts,omitempty"`
	NeedsArtifacts []NeedsArtifactDef `yaml:"needs_artifacts,omitempty"`
	// TestReports is a list of glob patterns the agent scans
	// after tasks complete. Each match is parsed as JUnit/xUnit
	// XML and the per-case results ship back as a TestResultBatch
	// alongside the JobResult. Empty list = no test reporting.
	TestReports []string  `yaml:"test_reports,omitempty"`
	Parallel    *Parallel `yaml:"parallel,omitempty"`
	Rules       []RuleDef `yaml:"rules,omitempty"`
	When        *WhenDef  `yaml:"when,omitempty"`
	Timeout     string    `yaml:"timeout,omitempty"`
	Retry       int       `yaml:"retry,omitempty"`
	Secrets     []string  `yaml:"secrets,omitempty"`
	Tags        []string  `yaml:"tags,omitempty"`
	// Agent picks a runner profile by name and may add extra tag
	// constraints. Profile resolution happens at apply time; the
	// resolved profile's tags merge (union) with the job's own
	// tags before scheduler matching, so neither origin can grant
	// itself a silent veto over the other.
	Agent *AgentDef `yaml:"agent,omitempty"`
	// Resources is the optional compute envelope. Empty fields fall
	// back to the resolved profile's defaults; non-empty fields are
	// validated against profile.max at apply time.
	Resources *ResourcesDef `yaml:"resources,omitempty"`
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
	// Outputs declares the structured k/v pairs this job promises
	// to produce. Each entry maps a YAML-alias to a plugin env-var
	// name read from $GOCDNEXT_OUTPUT_FILE at job end (same shape
	// as $GITHUB_OUTPUT). Downstream jobs reference any declared
	// alias via `${{ needs.<this-job>.outputs.<alias> }}` resolved
	// at dispatch. Empty/omitted = no outputs (the common case).
	// See issue #10.
	//
	// Two forms accepted (UnmarshalYAML on OutputDef):
	//
	//   outputs:
	//     # short form — alias: ENV_VAR (default masked: false)
	//     next: NEXT
	//     kind: KIND
	//     # object form — pin masking explicitly (issue #22)
	//     release-token:
	//       env: RELEASE_TOKEN
	//       masked: true        # bypass the scheduler 8-char heuristic for log scrubbing
	//
	// Masking semantics: outputs marked `masked: true` are added
	// to the downstream job's LogMasks at dispatch (same path
	// secrets take). Substitution scope is `env:` / `variables:`
	// / plugin `with:` — raw `script:` lines are NOT substituted
	// pre-dispatch (the shell resolves `${VAR}` at runtime from
	// the env we ship), so the script body never carries the
	// resolved value verbatim. Wherever the substituted value
	// DOES land (env exported into the container, plugin
	// settings echoed by the runner), the agent's log replacer
	// scrubs it. Downstream resolution of
	// `${{ needs.X.outputs.Y }}` still produces the real value;
	// the mask is for agent log streams only. Defence-in-depth:
	// the heuristic 8+-char auto-mask still applies for unmarked
	// outputs.
	//
	// 4-char floor: the agent's log replacer (applyMasks in
	// runner.go) skips masks shorter than 4 characters so common
	// short tokens ("v1", "go", "id") don't get globally rewritten.
	// `masked: true` bypasses the SCHEDULER's 8-char heuristic
	// but inherits the agent's 4-char floor — values shorter than
	// 4 chars are NOT scrubbed in log streams either way.
	// `secrets:` hits the same runner floor when echoed; the
	// honest recommendation for a short-and-sensitive value is to
	// NOT treat it as a build output at all — take it from
	// `secrets:` directly (stays off the outputs persistence +
	// downstream substitution surface) and avoid echoing it in
	// any step that prints env or argv.
	//
	// Scope of this release: log scrubbing only. UI rendering of
	// masked values is out of scope — the persisted output value
	// still propagates verbatim to downstream
	// `${{ needs.X.outputs.Y }}` substitutions and any future
	// outputs surface that reads job_runs.outputs directly.
	Outputs map[string]OutputDef `yaml:"outputs,omitempty"`
}

// OutputDef is the YAML shape of a single `outputs:` entry on a
// job. Two surface forms via the custom UnmarshalYAML:
//
//	# short form — same behaviour as v0.11.0
//	next: NEXT
//
//	# object form — opt-in fields (currently just `masked`)
//	release-token:
//	  env: RELEASE_TOKEN
//	  masked: true
//
// Internally normalised to the same struct; downstream code
// (parser, scheduler, dispatch) only sees the struct form.
type OutputDef struct {
	// Env is the plugin env-var name written to
	// $GOCDNEXT_OUTPUT_FILE that this alias maps to. Required.
	Env string `yaml:"env"`
	// Masked, when true, adds the resolved value to LogMasks at
	// dispatch so it gets scrubbed in downstream agent log
	// streams. The default (false) leaves the value plain — the
	// 8+-char heuristic auto-mask still applies as a backstop.
	// Explicit opt-in here is the operator declaring intent
	// ("this value IS sensitive"), tightening the contract
	// without breaking the heuristic-based defaults.
	Masked bool `yaml:"masked,omitempty"`
}

// UnmarshalYAML accepts both the short form ("alias: NEXT") and
// the object form ({env: NEXT, masked: true}). The short form is
// the v0.11.0 shape; pipelines that don't need masking stay
// unchanged. The object form is the v0.15.3+ opt-in for masking
// (issue #22).
//
// Strict on object-form keys: the outer parser uses
// yaml.Decoder.KnownFields(true) to fail loud on unknown keys,
// but that strictness does NOT propagate into a Node.Decode call
// inside an UnmarshalYAML. A typo like `mask: true` (missing
// `e`) would silently land as `masked=false` — exactly the kind
// of failure mode that should fail loud for a security-adjacent
// flag. We walk the mapping node manually so unknown keys
// terminate parsing instead of being dropped.
func (o *OutputDef) UnmarshalYAML(node *yaml.Node) error {
	// Scalar: short form. The YAML value is the env-var name.
	if node.Kind == yaml.ScalarNode {
		o.Env = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("outputs entry must be a string (env-var name) or an object {env, masked} — got YAML kind %d", node.Kind)
	}
	// Mapping: walk key/value pairs and validate keys before
	// decoding. node.Content alternates [key0, val0, key1, val1, ...].
	if len(node.Content)%2 != 0 {
		return fmt.Errorf("outputs entry mapping has odd number of nodes (malformed YAML)")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		val := node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("outputs entry key at line %d is not a scalar", key.Line)
		}
		switch key.Value {
		case "env":
			if err := val.Decode(&o.Env); err != nil {
				return fmt.Errorf("outputs entry `env` at line %d: %w", key.Line, err)
			}
		case "masked":
			if err := val.Decode(&o.Masked); err != nil {
				return fmt.Errorf("outputs entry `masked` at line %d: %w", key.Line, err)
			}
		default:
			return fmt.Errorf(
				"outputs entry has unknown key %q at line %d — accepted keys are `env` and `masked`. "+
					"Common typos: `mask` (missing `e`) or `env_var` (use `env`)",
				key.Value, key.Line)
		}
	}
	return nil
}

// ApprovalDef is the YAML shape of a manual approval gate. Kept
// explicit (`approval:` sub-object) rather than a top-level
// `type: approval` field because the other knobs (approvers,
// description) only make sense in the approval context — a
// sub-object keeps unrelated schema from bleeding across the job.
type ApprovalDef struct {
	Approvers      []string `yaml:"approvers,omitempty"`
	ApproverGroups []string `yaml:"approver_groups,omitempty"`
	// Required is the quorum: how many distinct allowed users must
	// approve before the gate passes. Default 1 (any single approver
	// is enough, matches legacy behaviour). Rejects always fail the
	// gate immediately regardless of this.
	Required int `yaml:"required,omitempty"`
	// QuorumByLabel maps a PR label name → quorum override applied
	// when the run is triggered by a PR carrying that label. See
	// domain.ApprovalSpec.QuorumByLabel for resolution semantics
	// (snapshot-at-materialisation, MAX wins on multi-label match).
	QuorumByLabel map[string]int `yaml:"quorum_by_label,omitempty"`
	Description   string         `yaml:"description,omitempty"`
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
	Paths        []string `yaml:"paths,omitempty"`         // subset filter; empty = all
	Dest         string   `yaml:"dest,omitempty"`          // default "./"
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

// AgentDef binds a job to a named runner_profiles row at apply
// time. `tags` here are extra constraints the user wants ON TOP of
// what the profile already requires — the resolved set is the union
// of both. Empty profile = legacy behaviour (any agent matching the
// job's own tags).
type AgentDef struct {
	Profile string   `yaml:"profile,omitempty"`
	Tags    []string `yaml:"tags,omitempty"`
}

// ResourcesDef mirrors corev1.ResourceRequirements but kept as
// strings so the YAML stays human-friendly ("100m", "256Mi"). The
// apply-time validator parses them into k8s quantities and checks
// against the resolved profile's max_cpu / max_mem.
type ResourcesDef struct {
	Requests *ResourceQty `yaml:"requests,omitempty"`
	Limits   *ResourceQty `yaml:"limits,omitempty"`
}

type ResourceQty struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}
