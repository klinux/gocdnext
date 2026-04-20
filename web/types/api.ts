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
