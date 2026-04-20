// RSC-only fetch helpers for /api/v1/admin/*. Kept in a separate
// module so the /settings pages don't drag the whole project/dashboard
// query surface in, and so a future permission check can be added in
// one place.

import { env } from "@/lib/env";
import type {
  AdminHealth,
  GitHubIntegration,
  RetentionSnapshot,
  WebhookDeliveriesResponse,
  WebhookDeliveryDetail,
} from "@/types/api";

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
    throw new Error(`GET ${url} returned ${res.status}: ${body.slice(0, 200)}`);
  }
  return (await res.json()) as T;
}

export async function getRetentionSnapshot(): Promise<RetentionSnapshot> {
  return readJSON<RetentionSnapshot>("/api/v1/admin/retention");
}

export async function getAdminHealth(): Promise<AdminHealth> {
  return readJSON<AdminHealth>("/api/v1/admin/health");
}

export async function getGitHubIntegration(): Promise<GitHubIntegration> {
  return readJSON<GitHubIntegration>("/api/v1/admin/integrations/github");
}

export type WebhookDeliveriesQuery = {
  provider?: string;
  status?: string;
  limit?: number;
  offset?: number;
};

export async function listWebhookDeliveries(
  opts: WebhookDeliveriesQuery = {},
): Promise<WebhookDeliveriesResponse> {
  const qs = new URLSearchParams({ limit: String(opts.limit ?? 50) });
  if (opts.offset) qs.set("offset", String(opts.offset));
  if (opts.provider) qs.set("provider", opts.provider);
  if (opts.status) qs.set("status", opts.status);
  return readJSON<WebhookDeliveriesResponse>(
    `/api/v1/admin/webhooks?${qs.toString()}`,
  );
}

export async function getWebhookDelivery(id: number): Promise<WebhookDeliveryDetail> {
  return readJSON<WebhookDeliveryDetail>(`/api/v1/admin/webhooks/${id}`);
}
