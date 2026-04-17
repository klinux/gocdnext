// RSC-only fetch helpers for the projects API. Cache strategy: no-store while
// the dashboard polls every tick; swap for tag-based revalidation once a
// realtime transport (SSE) is wired.

import { env } from "@/lib/env";
import type { ProjectDetail, ProjectSummary, RunDetail } from "@/types/api";

type ListResponse = { projects: ProjectSummary[] };

async function readJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  const res = await fetch(url, {
    cache: "no-store",
    ...init,
    headers: {
      Accept: "application/json",
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
