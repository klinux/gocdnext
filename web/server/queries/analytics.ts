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
  current: OrgMetrics;
  prior: OrgMetrics;
  daily: DoraDay[];
  teams: DoraGroup[];
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

export async function getDoraRollup(
  key: string,
  windowDays: number,
): Promise<DoraRollup> {
  return readJSON<DoraRollup>(
    `/api/v1/analytics/dora?key=${encodeURIComponent(key)}&window_days=${windowDays}`,
  );
}

// getDoraOverview is the single read behind the redesigned Analytics page.
export async function getDoraOverview(
  key: string,
  windowDays: number,
): Promise<DoraOverview> {
  return readJSON<DoraOverview>(
    `/api/v1/analytics/dora/overview?key=${encodeURIComponent(key)}&window_days=${windowDays}`,
  );
}
