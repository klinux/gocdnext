// DTOs mirroring server/internal/store read structs. Kept flat on purpose so
// that changing a field on one side is caught immediately by the compiler on
// the other. When the backend grows a proto-driven API these should be
// regenerated from the proto instead of hand-maintained.

export type ProjectSummary = {
  id: string;
  slug: string;
  name: string;
  description?: string;
  created_at: string;
  updated_at: string;
  pipeline_count: number;
  latest_run_at?: string;
};

export type PipelineSummary = {
  id: string;
  name: string;
  definition_version: number;
  updated_at: string;
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

export type ProjectDetail = {
  project: ProjectSummary;
  pipelines: PipelineSummary[];
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
};

export type VSMNode = {
  pipeline_id: string;
  name: string;
  definition_version: number;
  git_materials?: { url: string; branch?: string }[];
  latest_run?: RunSummary;
};

export type VSMEdge = {
  from_pipeline: string;
  to_pipeline: string;
  stage: string;
  status?: string;
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
