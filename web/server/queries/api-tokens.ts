// RSC-only fetch helpers for the per-user + per-SA token APIs.
// Same pattern as server/queries/projects.ts — forwards the
// session cookie so protected routes don't 401 in dev.
import { cookies } from "next/headers";

import { env } from "@/lib/env";

export type APIToken = {
  id: string;
  name: string;
  prefix: string;
  expires_at?: string | null;
  last_used_at?: string | null;
  revoked_at?: string | null;
  created_at: string;
};

export type ServiceAccount = {
  id: string;
  name: string;
  description: string;
  role: "admin" | "maintainer" | "viewer";
  created_by?: string | null;
  disabled_at?: string | null;
  created_at: string;
  updated_at: string;
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
    throw new Error(`GET ${url} returned ${res.status}`);
  }
  return (await res.json()) as T;
}

export async function listMyAPITokens(): Promise<APIToken[]> {
  const { tokens } = await readJSON<{ tokens: APIToken[] }>(
    "/api/v1/account/api-tokens",
  );
  return tokens;
}

export async function listServiceAccounts(): Promise<ServiceAccount[]> {
  const { service_accounts: accounts } = await readJSON<{
    service_accounts: ServiceAccount[];
  }>("/api/v1/admin/service-accounts");
  return accounts;
}

export async function listSATokens(saID: string): Promise<APIToken[]> {
  const { tokens } = await readJSON<{ tokens: APIToken[] }>(
    `/api/v1/admin/service-accounts/${encodeURIComponent(saID)}/tokens`,
  );
  return tokens;
}
