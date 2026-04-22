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
};

export type ProjectSCMInfo = {
  id: string;
  provider: "github" | "gitlab" | "bitbucket" | "manual";
  url: string;
  default_branch: string;
  auth_ref?: string;
};

export type RunSummary = {
  id: string;
  pipeline_id: string;
  pipeline_name: string;
  counter: number;
  cause: string;
  status: string;
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
  logs?: LogLine[];
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

export type Secret = {
  name: string;
  created_at: string;
  updated_at: string;
};

export type SecretsList = {
  secrets: Secret[];
  // Globals the project will resolve at runtime because no
  // same-name local secret shadows them. Read-only on the
  // project page — editing lives in /settings/secrets (admin).
  inherited?: Secret[];
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
  status: "accepted" | "rejected" | "error" | "ignored";
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
