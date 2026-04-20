// RSC-only fetch helpers for the projects API. Cache strategy: no-store while
// the dashboard polls every tick; swap for tag-based revalidation once a
// realtime transport (SSE) is wired.

import { cookies } from "next/headers";

import { env } from "@/lib/env";
import type {
  AgentDetail,
  AgentSummary,
  DashboardMetrics,
  GlobalRunSummary,
  ProjectDetail,
  ProjectSummary,
  ProjectVSM,
  RunDetail,
  RunsListResponse,
  SecretsList,
} from "@/types/api";

type ListResponse = { projects: ProjectSummary[] };

async function readJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  // Forward the browser's session cookie to the control plane so
  // protected RSC fetches don't 401 when auth is enabled. Missing
  // cookie = no header added (the control plane treats that as
  // anonymous, exactly like a fresh curl).
  const store = await cookies();
  const session = store.get("gocdnext_session")?.value;
  const res = await fetch(url, {
    cache: "no-store",
    ...init,
    headers: {
      Accept: "application/json",
      ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      ...(init?.headers ?? {}),
    },
  });
  if (!res.ok) {
    const body = await res.text();
    throw new GocdnextAPIError(
      `GET ${url} returned ${res.status}: ${body.slice(0, 200)}`,
      res.status,
    );
  }
  return (await res.json()) as T;
}

export class GocdnextAPIError extends Error {
  constructor(
    message: string,
    public readonly status: number,
  ) {
    super(message);
    this.name = "GocdnextAPIError";
  }
}

export async function listProjects(): Promise<ProjectSummary[]> {
  const { projects } = await readJSON<ListResponse>("/api/v1/projects");
  return projects;
}

export async function getProjectDetail(
  slug: string,
  runs = 25,
): Promise<ProjectDetail> {
  return readJSON<ProjectDetail>(
    `/api/v1/projects/${encodeURIComponent(slug)}?runs=${runs}`,
  );
}

export async function getRunDetail(
  id: string,
  logsPerJob = 200,
): Promise<RunDetail> {
  return readJSON<RunDetail>(
    `/api/v1/runs/${encodeURIComponent(id)}?logs=${logsPerJob}`,
  );
}

export async function getProjectVSM(slug: string): Promise<ProjectVSM> {
  return readJSON<ProjectVSM>(
    `/api/v1/projects/${encodeURIComponent(slug)}/vsm`,
  );
}

export async function getDashboardMetrics(): Promise<DashboardMetrics> {
  return readJSON<DashboardMetrics>("/api/v1/dashboard/metrics");
}

// RunsQuery mirrors the backend /api/v1/runs query string; every
// field optional so the dashboard widget passes just `limit` and
// the /runs page passes the full set.
export type RunsQuery = {
  limit?: number;
  offset?: number;
  status?: string;
  cause?: string;
  project?: string;
};

export async function listGlobalRuns(
  opts: RunsQuery = {},
): Promise<RunsListResponse> {
  const qs = new URLSearchParams({ limit: String(opts.limit ?? 20) });
  if (opts.offset) qs.set("offset", String(opts.offset));
  if (opts.status) qs.set("status", opts.status);
  if (opts.cause) qs.set("cause", opts.cause);
  if (opts.project) qs.set("project", opts.project);
  return readJSON<RunsListResponse>(`/api/v1/runs?${qs.toString()}`);
}

// listGlobalRunsOnly is the legacy shape the dashboard widget used
// (bare slice, no envelope). Kept so we don't tear up that code
// path in this slice — v2 can migrate it when convenient.
export async function listGlobalRunsOnly(limit = 20): Promise<GlobalRunSummary[]> {
  const env = await listGlobalRuns({ limit });
  return env.runs;
}

export async function listAgents(): Promise<AgentSummary[]> {
  const { agents } = await readJSON<{ agents: AgentSummary[] }>("/api/v1/agents");
  return agents;
}

export async function getAgentDetail(id: string): Promise<AgentDetail> {
  return readJSON<AgentDetail>(`/api/v1/agents/${encodeURIComponent(id)}`);
}

export async function listSecrets(slug: string) {
  const { secrets } = await readJSON<SecretsList>(
    `/api/v1/projects/${encodeURIComponent(slug)}/secrets`,
  );
  return secrets;
}
