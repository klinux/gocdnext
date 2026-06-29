// RSC-only fetch helpers for the cross-project analytics API (#107).

import { cookies } from "next/headers";

import { env } from "@/lib/env";

export type DoraGroup = {
  group: string;
  deploys_success: number;
  deploys_total: number;
  deploys_failed: number;
  deploy_freq_per_day: number;
  lead_time_p50_seconds: number;
  change_failure_rate: number;
  mttr_p50_seconds: number;
};

export type DoraRollup = {
  key: string;
  window_days: number;
  groups: DoraGroup[];
};

// Org-wide rollup over one window (no group label) — current + prior drive the
// hero cards' values and "vs. prior" deltas.
export type OrgMetrics = {
  deploys_success: number;
  deploys_total: number;
  deploys_failed: number;
  deploy_freq_per_day: number;
  lead_time_p50_seconds: number;
  change_failure_rate: number;
  mttr_p50_seconds: number;
};

export type DoraDay = {
  day: string; // YYYY-MM-DD
  deploys_success: number;
  deploys_total: number;
  deploys_failed: number;
  lead_time_p50_seconds: number;
};

// The single payload behind the Analytics page.
export type DoraOverview = {
  key: string;
  window_days: number;
  environment: string;
  current: OrgMetrics;
  prior: OrgMetrics;
  daily: DoraDay[];
  teams: DoraGroup[];
  teams_prior: DoraGroup[];
  bottleneck: LeadTimeBottleneck;
};

// Lead-time decomposition into four consecutive stages (p50 seconds), across
// successful deploys correlated to a PR via the merge SHA.
export type LeadTimeBottleneck = {
  correlated: number;
  excluded: number;
  coding_sample: number;
  review_sample: number;
  release_sample: number;
  deploy_sample: number;
  total_p50_seconds: number;
  coding_p50_seconds: number;
  review_p50_seconds: number;
  release_wait_p50_seconds: number;
  deploy_p50_seconds: number;
};

// Run-based throughput + reliability for one label-value group over the window.
// Distinct from DoraGroup (deploy-based) — these come from run history, so
// there's no environment dimension.
export type ThroughputGroup = {
  group: string;
  runs_success: number;
  runs_failed: number;
  runs_total: number;
  runs_per_day: number;
  success_rate: number;
  queue_wait_p50_seconds: number;
  duration_p50_seconds: number;
};

// One pipeline that fails often, among projects carrying the group-by key.
export type ReliabilityHotspot = {
  project_slug: string;
  project: string;
  pipeline: string;
  runs_total: number;
  runs_failed: number;
  failure_rate: number;
};

// The throughput & reliability payload behind that section of the Analytics page.
export type ReliabilityReport = {
  key: string;
  window_days: number;
  groups: ThroughputGroup[];
  hotspots: ReliabilityHotspot[];
};

// Compliance posture per label-value group — current state (framework adoption),
// so no window or environment.
export type FrameworkCoverage = {
  framework: string;
  covered: number;
};

export type ComplianceGroup = {
  group: string;
  projects_total: number;
  frameworks: FrameworkCoverage[];
};

export type ComplianceCoverageReport = {
  key: string;
  groups: ComplianceGroup[];
};

async function readJSON<T>(path: string): Promise<T> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  const session = (await cookies()).get("gocdnext_session")?.value;
  const res = await fetch(url, {
    cache: "no-store",
    headers: {
      Accept: "application/json",
      ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
    },
  });
  if (!res.ok) {
    const body = await res.text();
    throw new Error(`GET ${url} → ${res.status}: ${body.slice(0, 200)}`);
  }
  return (await res.json()) as T;
}

// listLabelKeys returns the distinct label keys available as a group-by
// dimension (team, tier, domain, …).
export async function listLabelKeys(): Promise<string[]> {
  const r = await readJSON<{ keys: string[] }>("/api/v1/analytics/label-keys");
  return r.keys ?? [];
}

// listEnvironments returns the deploy environments available as the environment
// filter, scoped to the group-by key.
export async function listEnvironments(key: string): Promise<string[]> {
  const r = await readJSON<{ environments: string[] }>(
    `/api/v1/analytics/environments?key=${encodeURIComponent(key)}`,
  );
  return r.environments ?? [];
}

export async function getDoraRollup(
  key: string,
  windowDays: number,
): Promise<DoraRollup> {
  return readJSON<DoraRollup>(
    `/api/v1/analytics/dora?key=${encodeURIComponent(key)}&window_days=${windowDays}`,
  );
}

// getDoraOverview is the single read behind the redesigned Analytics page.
// environment "" means all environments.
export async function getDoraOverview(
  key: string,
  windowDays: number,
  environment = "",
): Promise<DoraOverview> {
  const env = environment ? `&environment=${encodeURIComponent(environment)}` : "";
  return readJSON<DoraOverview>(
    `/api/v1/analytics/dora/overview?key=${encodeURIComponent(key)}&window_days=${windowDays}${env}`,
  );
}

// getReliability reads the run-based throughput + reliability rollup. No
// environment filter — runs aren't environment-scoped.
export async function getReliability(
  key: string,
  windowDays: number,
): Promise<ReliabilityReport> {
  return readJSON<ReliabilityReport>(
    `/api/v1/analytics/reliability?key=${encodeURIComponent(key)}&window_days=${windowDays}`,
  );
}

// getComplianceCoverage reads framework adoption per label-value group (current
// state — no window).
export async function getComplianceCoverage(
  key: string,
): Promise<ComplianceCoverageReport> {
  return readJSON<ComplianceCoverageReport>(
    `/api/v1/analytics/compliance?key=${encodeURIComponent(key)}`,
  );
}
