// DTOs mirroring server/internal/store read structs. Kept flat on purpose so
// that changing a field on one side is caught immediately by the compiler on
// the other. When the backend grows a proto-driven API these should be
// regenerated from the proto instead of hand-maintained.

export type UserPreferences = {
  hidden_projects?: string[];
};

export type UserPreferencesResponse = {
  preferences: UserPreferences;
  updated_at?: string;
};

export type CurrentUser = {
  id: string;
  email: string;
  name: string;
  avatar_url?: string;
  provider: string;
  external_id: string;
  role: "admin" | "user" | "viewer";
  last_login_at?: string;
  created_at: string;
  updated_at: string;
};

export type AuthProvidersResponse = {
  enabled: boolean;
  providers: { name: string; display: string }[];
  local_enabled?: boolean;
};

export type PipelinePreview = {
  id: string;
  name: string;
  latest_run_status?: string;
  latest_run_at?: string;
  definition_stages?: string[];
  latest_run_stages?: StageRunSummary[];
};

export type ProjectStatus =
  | "no_pipelines"
  | "never_run"
  | "running"
  | "failing"
  | "success";

export type ProjectProvider =
  | "github"
  | "gitlab"
  | "bitbucket"
  | "manual"
  | ""; // empty = no scm_source bound yet

// ProjectLabel is a free-form key:value grouping tag (team:payments,
// tier:critical) — the primitive cross-project views group/filter by.
export type ProjectLabel = {
  key: string;
  value: string;
};

export type ProjectSummary = {
  id: string;
  slug: string;
  name: string;
  description?: string;
  config_path?: string;
  created_at: string;
  updated_at: string;
  pipeline_count: number;
  run_count: number;
  latest_run_at?: string;
  provider?: ProjectProvider;
  status: ProjectStatus;
  top_pipelines?: PipelinePreview[];
  metrics?: PipelineMetrics;
  latest_run_meta?: RunMeta;
  labels?: ProjectLabel[];
};

export type ProjectSCMInfo = {
  id: string;
  provider: "github" | "gitlab" | "bitbucket" | "manual";
  url: string;
  default_branch: string;
  auth_ref?: string;
  // poll_interval_ns is the project-level fallback applied to
  // the synthesized implicit material. 0 disables.
  poll_interval_ns?: number;
};

export type RunSummary = {
  id: string;
  pipeline_id: string;
  pipeline_name: string;
  counter: number;
  cause: string;
  status: string;
  // Snapshot of `pipeline.Services` non-emptiness stamped at
  // run-create time (server migration 00036). The project page
  // gates the per-card services fetch on this — without it, every
  // pipeline card polls /api/v1/runs/:id/services even when the
  // pipeline never declared a `services:` block.
  has_services: boolean;
  // Names of the services snapshotted at run-create (server migration
  // 00055) — the name-granular companion to has_services, so the
  // pipelines list can label declared services without the per-card
  // /services fetch. Empty array when the run declared none.
  service_names: string[];
  // Set when a run was canceled: the operator-visible reason. For a supersede
  // (#97) it's "superseded by #N" (counter only) — the UI renders it as a muted
  // badge in the canceled tone. Absent for runs that weren't canceled.
  cancel_reason?: string;
  // The id of the newer run that superseded this one (present only for a
  // supersede-cancel; the winning run may be GC'd, so it can be absent even when
  // cancel_reason is set). Lets the "superseded by #N" badge link to the winner.
  superseded_by?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  triggered_by?: string;
};

export type JobRunSummaryLite = {
  id: string;
  name: string;
  status: string;
  started_at?: string;
  finished_at?: string;
};

export type StageRunSummary = {
  id: string;
  name: string;
  ordinal: number;
  status: string;
  started_at?: string;
  finished_at?: string;
  jobs?: JobRunSummaryLite[];
};

export type DefinitionJob = {
  name: string;
  stage: string;
};

export type PipelineSummary = {
  id: string;
  name: string;
  definition_version: number;
  updated_at: string;
  definition_stages?: string[];
  definition_jobs?: DefinitionJob[];
  latest_run?: RunSummary;
  latest_run_stages?: StageRunSummary[];
  metrics?: PipelineMetrics;
  latest_run_meta?: RunMeta;
};

export type RunMeta = {
  revision?: string;
  branch?: string;
  message?: string;
  author?: string;
  triggered_by?: string;
  // cause + pr_number let cards show a PR reference (PR #N) instead of a
  // branch when the latest run was triggered by a pull_request.
  cause?: string;
  pr_number?: number;
};

export type PipelineMetrics = {
  window_days: number;
  runs_considered: number;
  success_rate: number; // 0..1
  lead_time_p50_seconds: number;
  process_time_p50_seconds: number;
  stage_stats?: StageStat[];
};

export type StageStat = {
  name: string;
  runs_considered: number;
  success_rate: number; // 0..1
  duration_p50_seconds: number;
};

export type PipelineEdge = {
  from_pipeline: string;
  to_pipeline: string;
  stage?: string;
  status?: string;
};

export type ProjectDetail = {
  project: ProjectSummary;
  scm_source?: ProjectSCMInfo;
  pipelines: PipelineSummary[];
  edges?: PipelineEdge[];
  runs: RunSummary[];
};

export type LogLine = {
  seq: number;
  stream: string;
  at: string;
  text: string;
};

export type JobDetail = {
  id: string;
  stage_run_id: string;
  name: string;
  matrix_key?: string;
  image?: string;
  status: string;
  exit_code?: number;
  error?: string;
  started_at?: string;
  finished_at?: string;
  agent_id?: string;
  // cancel_requested_at is non-null when the operator hit Cancel
  // but the agent hasn't acknowledged yet (deferred cancel — the
  // server stamps it in the same tx as the SELECT FOR UPDATE so
  // it survives Revoke→Register session churn). Combined with
  // status==="running" it tells the UI to render a "Canceling…"
  // badge instead of the regular running spinner — operators see
  // the request landed even when the agent is mid-task.
  cancel_requested_at?: string;
  // logs carries the TAIL of the job log (oldest-first within the
  // returned window). Name kept for backward compat with callers
  // that pre-date head+omitted.
  logs?: LogLine[];
  // logs_head carries the FIRST N lines when the run-detail request
  // included `?head=N`. Together with `logs_omitted`, the UI renders
  // long jobs as "start + (X lines omitted) + end" so the startup
  // phase (Gradle daemon banner, dependency resolution, JDK
  // selection) survives the tail-only cap.
  logs_head?: LogLine[];
  // logs_omitted is the count of lines neither in `logs_head` nor
  // in `logs`. Zero when head+tail covers everything (short jobs)
  // and zero when the caller didn't request head. The UI shows
  // this as a divider between head and tail.
  logs_omitted?: number;

  // Approval-gate metadata. Populated only when the job is a
  // manual approval gate; the server omits these fields on
  // regular jobs so they stay absent on the wire.
  approval_gate?: boolean;
  approvers?: string[];
  approval_required?: number;
  approval_description?: string;
  // approval_quorum_label names the PR label whose
  // quorum_by_label override fired when this gate was
  // materialised. Omitted entirely when no override fired
  // (regular gate, push-cause run, or PR with non-matching
  // labels) so the UI can render a discreet badge only on the
  // actual policy events.
  approval_quorum_label?: string;
  awaiting_since?: string;
  decided_by?: string;
  decided_at?: string;
  decision?: "approved" | "rejected" | string;

  // Notification-job metadata. Populated only for jobs in the
  // synthetic `_notifications` stage; the UI keys off these to
  // render a friendly label (notify_uses) and a trigger pill
  // (notify_on) instead of the raw `_notify_<idx>` slug.
  notify_on?: "failure" | "success" | "always" | "canceled" | string;
  notify_uses?: string;
};

export type StageDetail = {
  id: string;
  name: string;
  ordinal: number;
  status: string;
  started_at?: string;
  finished_at?: string;
  jobs: JobDetail[];
};

export type RunDetail = RunSummary & {
  project_slug: string;
  cause_detail?: Record<string, unknown>;
  revisions?: Record<string, { revision: string; branch: string }>;
  stages: StageDetail[];
};

// SecretSource names where a secret's value comes from at runtime.
// "db" is the encrypted-at-rest value managed in-app; the others are
// external backends the server resolves on demand (the value never
// touches our DB). `ref` is present only for external sources.
export type SecretSource = "db" | "vault" | "gcp" | "aws";

export type SecretRef = {
  path: string;
  // key is required for Vault (a path holds a map of keys) and
  // optional for the others (the path addresses a single value).
  key?: string;
};

export type Secret = {
  name: string;
  source: SecretSource;
  // Present only for external sources — the address in the backend.
  ref?: SecretRef;
  created_at: string;
  updated_at: string;
};

export type SecretsList = {
  secrets: Secret[];
  // Pagination envelope: the server echoes the absolute total +
  // the limit/offset it applied so the UI can render pagination
  // controls without a follow-up count query.
  total: number;
  limit: number;
  offset: number;
  // Globals the project will resolve at runtime because no
  // same-name local secret shadows them. Read-only on the
  // project page — editing lives in /settings/secrets (admin).
  inherited?: Secret[];
  // External backends the server has enabled. The dialog gates its
  // source options on ["db", ...configured_sources] so an operator
  // can't pick a backend the control plane can't resolve.
  configured_sources: string[];
};

// SecretBackendSource is the subset of SecretSource that has an
// admin-configurable backend connection (db is in-app, no backend).
export type SecretBackendSource = "vault" | "gcp" | "aws";

// SecretBackend mirrors a row from GET /api/v1/admin/secret-backends.
// `value` carries only NON-secret connection config (addr, region,
// project, …). Credential VALUES never cross the wire —
// `credential_keys` is ["configured"] when a credential is stored
// (render a "•••• stored" badge), else []. It may also be `null`: the
// server field is a Go []string and a backend saved with no credential
// (e.g. GCP with project only) marshals nil as JSON null — readers must
// treat null as "none". `source_origin` distinguishes a saved DB
// override ("db", editable/deletable) from the env baseline ("env").
export type SecretBackend = {
  source: SecretBackendSource;
  enabled: boolean;
  value: Record<string, unknown>;
  credential_keys: string[] | null;
  source_origin: "db" | "env";
  updated_at?: string;
};

export type SecretBackendsList = {
  // Always three entries (vault, gcp, aws) regardless of config state.
  backends: SecretBackend[];
};

// SecretBackendProbeResult is the POST .../test response. Always HTTP
// 200; `status` carries the outcome and `message` an optional detail.
export type SecretBackendProbeResult = {
  status: "ok" | "unauthorized" | "unreachable" | "error";
  message?: string;
};

export type CacheSummary = {
  id: string;
  key: string;
  size_bytes: number;
  status: "pending" | "ready" | string;
  content_sha256?: string;
  created_at: string;
  updated_at: string;
  last_accessed_at: string;
};

export type CachesList = {
  caches: CacheSummary[];
  // Sum of size_bytes across `ready` rows only — matches what
  // the sweeper sees on disk, so the UI's "footprint" number
  // and the sweeper's quota logic stay in sync.
  total_bytes: number;
};

// PluginInput mirrors one row of a `plugin.yaml`'s `inputs:` map
// as the UI sees it — the catalog endpoint surfaces sorted arrays
// (not objects) so the docs page iteration is stable.
export type PluginInput = {
  name: string;
  required: boolean;
  default?: string;
  description?: string;
};

export type PluginExample = {
  name?: string;
  description?: string;
  yaml: string;
};

export type PluginSummary = {
  name: string;
  description?: string;
  // Category groups plugins on the UI (build/container/security/
  // deploy/notifications/release). Empty on legacy manifests.
  category?: string;
  inputs: PluginInput[];
  examples?: PluginExample[];
};

export type PluginsList = {
  plugins: PluginSummary[];
};

// Admin: user list + audit events.
export type AdminUser = {
  id: string;
  email: string;
  name: string;
  avatar_url?: string;
  provider: string;
  external_id: string;
  role: "admin" | "maintainer" | "viewer" | string;
  disabled_at?: string;
  last_login_at?: string;
  created_at: string;
  updated_at: string;
};

export type UsersList = {
  users: AdminUser[];
};

export type AuditEventRow = {
  id: string;
  actor_id?: string;
  actor_email?: string;
  action: string;
  target_type: string;
  target_id?: string;
  metadata: Record<string, unknown>;
  at: string;
};

export type AuditEventsList = {
  events: AuditEventRow[];
  // Pagination envelope: the server returns the absolute total
  // + the echoed limit/offset so the UI can render pagination
  // controls without a follow-up count query.
  total: number;
  limit: number;
  offset: number;
};

export type VSMNode = {
  pipeline_id: string;
  name: string;
  definition_version: number;
  git_materials?: { url: string; branch?: string }[];
  latest_run?: RunSummary;
  metrics?: PipelineMetrics;
};

export type VSMEdge = {
  from_pipeline: string;
  to_pipeline: string;
  stage: string;
  status?: string;
  wait_time_p50_seconds?: number;
  wait_samples?: number;
};

export type ProjectVSM = {
  project_id: string;
  project_slug: string;
  project_name: string;
  nodes: VSMNode[];
  edges: VSMEdge[];
  generated_at: string;
};

export type DashboardMetrics = {
  runs_today: number;
  successes_7d: number;
  failures_7d: number;
  canceled_7d: number;
  success_rate_7d: number; // 0..1
  p50_seconds_7d: number;
  queued_runs: number;
  pending_jobs: number;
};

export type GlobalRunSummary = RunSummary & {
  project_id: string;
  project_slug: string;
  project_name: string;
};

export type AgentJobSummary = {
  job_run_id: string;
  job_name: string;
  job_status: string;
  started_at?: string;
  finished_at?: string;
  exit_code?: number;
  run_id: string;
  run_counter: number;
  pipeline_name: string;
  project_id: string;
  project_slug: string;
  project_name: string;
};

export type AgentDetail = {
  agent: AgentSummary;
  jobs: AgentJobSummary[];
};

export type RunsListResponse = {
  runs: GlobalRunSummary[];
  total: number;
  limit: number;
  offset: number;
};

export type AgentSummary = {
  id: string;
  name: string;
  version?: string;
  os?: string;
  arch?: string;
  tags: string[];
  capacity: number;
  status: string;
  health_state: "online" | "stale" | "offline" | "idle";
  last_seen_at?: string;
  registered_at: string;
  running_jobs: number;
};

export type WebhookDeliverySummary = {
  id: number;
  provider: string;
  event: string;
  material_id?: string;
  // "skipped" = valid push suppressed by a [skip ci] commit marker.
  status: "accepted" | "rejected" | "error" | "ignored" | "skipped";
  http_status: number;
  error?: string;
  received_at: string;
};

export type WebhookDeliveryDetail = WebhookDeliverySummary & {
  headers?: Record<string, string>;
  payload?: unknown;
};

export type WebhookDeliveriesResponse = {
  deliveries: WebhookDeliverySummary[];
  total: number;
  limit: number;
  offset: number;
};

export type RetentionSnapshot =
  | { enabled: false }
  | {
      enabled: true;
      tick: number;
      batch_size: number;
      grace_minutes: number;
      keep_last: number;
      project_quota_bytes: number;
      global_quota_bytes: number;
      last_sweep_at?: string;
      last_stats: {
        DemotedKeepLast: number;
        DemotedProjectCap: number;
        DemotedGlobalCap: number;
        Claimed: number;
        Deleted: number;
        StorageFailures: number;
        DBFailures: number;
        BytesFreed: number;
      };
    };

export type AdminHealth = {
  db_ok: boolean;
  db_error?: string;
  agents_online: number;
  agents_stale: number;
  agents_offline: number;
  queued_runs: number;
  pending_jobs: number;
  success_rate_7d: number;
  checked_at: string;
};

export type ConfiguredAuthProvider = {
  id: string;
  name: string;
  kind: "github" | "oidc";
  display_name: string;
  client_id: string;
  issuer?: string;
  github_api_base?: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
};

export type AuthProvidersAdmin = {
  enabled: boolean;
  providers: ConfiguredAuthProvider[];
  env_only: string[];
};

// DB-managed VCS integration row. Secrets never cross the wire —
// has_private_key / has_webhook_secret tell the UI whether the
// stored ciphertext exists so the dialog can render "••••".
export type ConfiguredVCSIntegration = {
  id: string;
  kind: "github_app";
  name: string;
  display_name: string;
  app_id?: number;
  api_base?: string;
  enabled: boolean;
  has_private_key: boolean;
  has_webhook_secret: boolean;
  created_at: string;
  updated_at: string;
};

// Active registry view. Source=env rows are read-only in the UI;
// source=db rows render edit/delete controls.
export type ActiveVCSIntegration = {
  id?: string;
  name: string;
  kind: string;
  display_name?: string;
  app_id?: number;
  api_base?: string;
  enabled: boolean;
  source: "env" | "db";
  updated_at?: string;
};

export type VCSIntegrationsAdmin = {
  integrations: ConfiguredVCSIntegration[];
  active: ActiveVCSIntegration[];
};

export type GitHubIntegration = {
  github_app_configured: boolean;
  public_base_set: boolean;
  checks_reporter_on: boolean;
  auto_register_on: boolean;
};

export type SCMCredential = {
  id: string;
  provider: "gitlab" | "bitbucket";
  host: string;
  api_base?: string;
  display_name?: string;
  auth_ref_preview?: string;
  created_at: string;
  updated_at: string;
};

export type SCMCredentialsList = {
  credentials: SCMCredential[];
};

export type IntegrationsSummary = {
  public_base: string;
  public_base_set: boolean;
  github: {
    app_configured: boolean;
    checks_reporter_on: boolean;
    auto_register_on: boolean;
    webhook_endpoint: string;
  };
  gitlab: {
    auto_register_on: boolean;
    webhook_endpoint: string;
    required_scope: string;
  };
  bitbucket: {
    auto_register_on: boolean;
    webhook_endpoint: string;
    required_scope: string;
  };
};

export type TestCaseStatus = "passed" | "failed" | "skipped" | "errored";

export type TestCase = {
  id: string;
  job_run_id: string;
  suite: string;
  classname?: string;
  name: string;
  status: TestCaseStatus | string;
  duration_ms: number;
  failure_type?: string;
  failure_message?: string;
  failure_detail?: string;
};

export type TestSummary = {
  job_run_id: string;
  total: number;
  passed: number;
  failed: number;
  skipped: number;
  errored: number;
  duration_ms: number;
};

export type TestResultsResponse = {
  summaries: TestSummary[];
  cases: TestCase[];
};

export type TestCaseHistoryEntry = {
  id: string;
  run_id: string;
  run_counter: number;
  pipeline_name: string;
  project_slug: string;
  status: TestCaseStatus | string;
  duration_ms: number;
  failure_message?: string;
  at: string;
};

export type TestCaseHistoryResponse = {
  entries: TestCaseHistoryEntry[];
};

export type RunArtifact = {
  id: string;
  job_run_id: string;
  job_name: string;
  path: string;
  status: "pending" | "ready" | "deleting";
  size_bytes: number;
  content_sha256: string;
  created_at: string;
  expires_at?: string;
  download_url?: string;
  download_url_expires_at?: string;
};

// RunService is one row per pipeline-level service tracked in
// service_runs. Status follows the agent emission order:
// starting → ready → stopped, with `failed` as the terminal
// branch when the pod never reached Running. Empty array on
// the wire for runs without services.
export type RunService = {
  id: string;
  name: string;
  image: string;
  pod_name?: string;
  status: "starting" | "ready" | "stopped" | "failed";
  started_at?: string;
  ready_at?: string;
  stopped_at?: string;
  error?: string;
};

// OIDCKey is the lifecycle view of one id_tokens signing key, as
// served by GET /api/v1/admin/oidc/keys. Key material (even the
// public DER) never crosses this endpoint — the JWKS is the only
// public-key surface; the admin UI shows lifecycle.
// active = neither retired_at nor revoked_at;
// retired  = still in the JWKS until in-flight tokens expire;
// revoked  = emergency-rotated out, gone from the JWKS.
export type OIDCKey = {
  id: string;
  kid: string;
  alg: string;
  created_at: string;
  retired_at?: string;
  revoked_at?: string;
};

export type OIDCKeysList = { keys: OIDCKey[] };

// Deployment tracking (#39). A DeploymentRecord is one recorded
// deploy attempt against an environment. run_id is absent once the
// run is garbage-collected (the record survives as an audit fact, so
// the UI degrades the run link). `current` on an environment is the
// newest successful deploy, or null when nothing has shipped there
// yet. Same shape for "current" and history rows.
export type DeploymentRecord = {
  id: string;
  run_id?: string;
  attempt: number;
  version: string;
  status: "in_progress" | "success" | "failed";
  is_rollback: boolean;
  deployed_by?: string;
  created_at: string;
  finished_at?: string;
};

export type EnvironmentSummary = {
  id: string;
  name: string;
  description?: string;
  created_at: string;
  updated_at: string;
  current: DeploymentRecord | null;
};

export type EnvironmentsList = { environments: EnvironmentSummary[] };

export type DeploymentsList = { deployments: DeploymentRecord[] };

// Security finding ingested from a SARIF scanner artifact (#71).
export type Finding = {
  id: number;
  pipeline_id: string;
  run_id: string;
  job_name: string;
  tool: string;
  rule_id: string;
  severity: string; // critical|high|medium|low
  level: string;
  message: string;
  location_path: string;
  location_line: number;
  location_url: string;
  artifact_id?: string | null;
  artifact_path: string;
  created_at: string;
  status: string; // "new" (first seen in this run) | "existing"
  state: string; // open | dismissed | false_positive | accepted
  state_id: number; // security_finding_states identity id (0 if absent)
  state_reason: string;
};

// FixedFinding is an identity gone from the scanner's latest scan — surfaced
// from the snapshot (its security_findings occurrence row no longer exists).
export type FixedFinding = {
  id: number;
  pipeline_id: string;
  scanner_job: string;
  matrix_key: string;
  tool: string;
  rule_id: string;
  severity: string;
  level: string;
  message: string;
  location_path: string;
  location_line: number;
  last_seen_at: string;
};

export type FindingsList = {
  findings: Finding[];
  total: number;
  severity_counts: Record<string, number>;
  accepted_count: number;
  fixed: FixedFinding[];
  fixed_total: number;
  limit: number;
  offset: number;
};
