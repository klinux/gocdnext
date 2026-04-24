// RSC-only fetch helpers for /api/v1/admin/*. Kept in a separate
// module so the /settings pages don't drag the whole project/dashboard
// query surface in, and so a future permission check can be added in
// one place.

import { cookies } from "next/headers";

import { env } from "@/lib/env";
import type {
  AdminHealth,
  AuditEventsList,
  AuthProvidersAdmin,
  GitHubIntegration,
  RetentionSnapshot,
  SecretsList,
  UsersList,
  VCSIntegrationsAdmin,
  WebhookDeliveriesResponse,
  WebhookDeliveryDetail,
} from "@/types/api";

async function readJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  // Forward the session cookie so the control plane's RequireRole
  // middleware sees the admin user on admin routes.
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

export async function listConfiguredAuthProviders(): Promise<AuthProvidersAdmin> {
  return readJSON<AuthProvidersAdmin>("/api/v1/admin/auth/providers");
}

export async function listVCSIntegrations(): Promise<VCSIntegrationsAdmin> {
  return readJSON<VCSIntegrationsAdmin>("/api/v1/admin/integrations/vcs");
}

// listGlobalSecrets fetches the names + timestamps of every global
// (unscoped) secret. Values never cross the wire — the runtime
// resolver is the only reader. Returns [] when the subsystem is
// up and no globals exist yet; the 503 path (GOCDNEXT_SECRET_KEY
// unset) propagates as an error so the page can render a distinct
// "subsystem disabled" state.
export async function listGlobalSecrets(): Promise<SecretsList["secrets"]> {
  const { secrets } = await readJSON<SecretsList>("/api/v1/admin/secrets");
  return secrets;
}

export async function listAdminUsers(): Promise<UsersList> {
  return readJSON<UsersList>("/api/v1/admin/users");
}

export async function listAuditEvents(
  params?: {
    action?: string;
    targetType?: string;
    actor?: string;
    limit?: number;
  },
): Promise<AuditEventsList> {
  const q = new URLSearchParams();
  if (params?.action) q.set("action", params.action);
  if (params?.targetType) q.set("target_type", params.targetType);
  if (params?.actor) q.set("actor", params.actor);
  if (params?.limit) q.set("limit", String(params.limit));
  const suffix = q.toString() ? `?${q.toString()}` : "";
  return readJSON<AuditEventsList>(`/api/v1/admin/audit${suffix}`);
}
