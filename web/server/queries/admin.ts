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
  IntegrationsSummary,
  OIDCKeysList,
  RetentionSnapshot,
  SCMCredentialsList,
  SecretBackend,
  SecretBackendsList,
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

export async function getIntegrationsSummary(): Promise<IntegrationsSummary> {
  return readJSON<IntegrationsSummary>("/api/v1/admin/integrations");
}

export async function listSCMCredentials(): Promise<SCMCredentialsList> {
  return readJSON<SCMCredentialsList>("/api/v1/admin/scm-credentials");
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

// SecretsQuery is the pagination window for the global secrets list.
export type SecretsQuery = {
  limit?: number;
  offset?: number;
};

// listGlobalSecrets fetches the names + source + timestamps of every
// global (unscoped) secret. Values never cross the wire — the runtime
// resolver is the only reader. Returns an envelope with total=0 when
// the subsystem is up and no globals exist yet; the 503 path
// (GOCDNEXT_SECRET_KEY unset) propagates as an error so the page can
// render a distinct "subsystem disabled" state.
export async function listGlobalSecrets(
  opts: SecretsQuery = {},
): Promise<SecretsList> {
  const qs = new URLSearchParams({ limit: String(opts.limit ?? 50) });
  if (opts.offset) qs.set("offset", String(opts.offset));
  return readJSON<SecretsList>(`/api/v1/admin/secrets?${qs.toString()}`);
}

// StorageConfig is what the /api/v1/admin/storage GET returns.
// `value` carries non-secret backend config; `credential_keys`
// is a name-only list (server never echoes credential VALUES).
// `source` distinguishes the DB override from the env fallback —
// the UI shows a different banner per source.
export type StorageConfig = {
  backend: "filesystem" | "s3" | "gcs";
  value: Record<string, unknown>;
  credential_keys: string[];
  updated_at?: string;
  updated_by?: string;
  source: "db" | "env";
};

export async function getStorageConfig(): Promise<StorageConfig> {
  return readJSON<StorageConfig>("/api/v1/admin/storage");
}

// listSecretBackends fetches the three admin-configurable external
// secret backends (vault, gcp, aws). Credential VALUES never cross the
// wire — `credential_keys` is a presence marker only. The endpoint
// always returns all three entries regardless of config state, so the
// editor renders one panel per backend without a follow-up call.
export async function listSecretBackends(): Promise<SecretBackend[]> {
  const list = await readJSON<SecretBackendsList>(
    "/api/v1/admin/secret-backends",
  );
  return list.backends;
}

export async function listAdminUsers(): Promise<UsersList> {
  return readJSON<UsersList>("/api/v1/admin/users");
}

export type AdminGroup = {
  id: string;
  name: string;
  description: string;
  member_count: number;
  created_by?: string;
  created_at: string;
  updated_at: string;
};

export type AdminGroupMember = {
  user_id: string;
  email: string;
  name: string;
  role: string;
  added_at: string;
};

export async function listAdminGroups(): Promise<{ groups: AdminGroup[] }> {
  return readJSON<{ groups: AdminGroup[] }>("/api/v1/admin/groups");
}

export type AdminRunnerProfile = {
  id: string;
  name: string;
  description: string;
  engine: string;
  default_image: string;
  default_cpu_request: string;
  default_cpu_limit: string;
  default_mem_request: string;
  default_mem_limit: string;
  max_cpu: string;
  max_mem: string;
  tags: string[];
  config?: Record<string, unknown>;
  // Plain env vars the runner injects into plugin containers
  // running on this profile.
  env: Record<string, string>;
  // Names of encrypted secrets configured on this profile. Values
  // never leave the server through this endpoint — the UI shows a
  // masked indicator next to each key so the admin knows a value
  // exists without seeing it.
  secret_keys: string[];
  // Map of {key → global secret NAME} for secrets whose stored
  // value is a single `{{secret:NAME}}` template. Lets the editor
  // render the chip "→ globals.NAME" instead of the masked
  // placeholder for clean references.
  secret_refs: Record<string, string>;
  // Scheduling hints honoured by the Kubernetes engine (shipped
  // v0.14.0). node_selector merges with the agent-level
  // nodeSelector (profile wins on key collision); tolerations
  // append. Always emitted as `{}` / `[]` by the server, never
  // null, so the editor can iterate without nil-checks.
  node_selector: Record<string, string>;
  tolerations: AdminToleration[];
  created_at: string;
  updated_at: string;
};

// AdminToleration mirrors corev1.Toleration on the wire. operator
// is the explicit normalised value on read (empty input becomes
// "Equal" server-side before persistence). toleration_seconds is
// only honoured with effect=NoExecute.
export type AdminToleration = {
  key?: string;
  operator: "Equal" | "Exists";
  value?: string;
  effect?: "" | "NoSchedule" | "PreferNoSchedule" | "NoExecute";
  toleration_seconds?: number | null;
};

export async function listAdminRunnerProfiles(): Promise<{
  profiles: AdminRunnerProfile[];
}> {
  return readJSON<{ profiles: AdminRunnerProfile[] }>(
    "/api/v1/admin/runner-profiles",
  );
}

// AdminCluster mirrors the GET /api/v1/admin/clusters row shape. The
// credential (kubeconfig blob / bearer token) is write-only and NEVER
// crosses the wire — the editor starts it blank and sends the preserve
// sentinel to keep the stored value on update. The CA cert, by
// contrast, is a PUBLIC certificate (no private key): the endpoint DOES
// echo it (`ca_cert`) so the editor can prefill it on edit and re-send
// it, instead of silently dropping it and degrading token auth to
// insecure TLS. `has_ca` is the cheap boolean for list rendering.
export type AdminCluster = {
  id: string;
  name: string;
  description: string;
  auth_type: "kubeconfig" | "token" | "in_cluster";
  api_server: string;
  has_ca: boolean;
  ca_cert: string;
  // Project IDs allowed to target this cluster. Always emitted as an
  // array by the server (never null) so the editor iterates without a
  // nil-check.
  allowed_projects: string[];
  created_by?: string;
  created_at: string;
  updated_at: string;
};

// The endpoint returns a bare array (no envelope), so we read it as
// AdminCluster[] directly.
export async function listAdminClusters(): Promise<AdminCluster[]> {
  return readJSON<AdminCluster[]>("/api/v1/admin/clusters");
}

export async function listAdminGroupMembers(
  groupID: string,
): Promise<{ members: AdminGroupMember[] }> {
  return readJSON<{ members: AdminGroupMember[] }>(
    `/api/v1/admin/groups/${encodeURIComponent(groupID)}/members`,
  );
}

// listOIDCKeys returns every id_tokens signing key (active +
// retired + revoked), newest first. Lifecycle metadata only — key
// material never crosses the admin API.
export async function listOIDCKeys(): Promise<OIDCKeysList> {
  return readJSON<OIDCKeysList>("/api/v1/admin/oidc/keys");
}

export async function listAuditEvents(
  params?: {
    action?: string;
    targetType?: string;
    actor?: string;
    from?: string;
    to?: string;
    limit?: number;
    offset?: number;
  },
): Promise<AuditEventsList> {
  const q = new URLSearchParams();
  if (params?.action) q.set("action", params.action);
  if (params?.targetType) q.set("target_type", params.targetType);
  if (params?.actor) q.set("actor", params.actor);
  if (params?.from) q.set("from", params.from);
  if (params?.to) q.set("to", params.to);
  if (params?.limit) q.set("limit", String(params.limit));
  if (params?.offset) q.set("offset", String(params.offset));
  const suffix = q.toString() ? `?${q.toString()}` : "";
  return readJSON<AuditEventsList>(`/api/v1/admin/audit${suffix}`);
}

// --- Compliance (frameworks + policies) -----------------------------------
// Types mirror the admin REST DTOs (server/internal/api/admin/compliance.go).
// Co-located with the queries, same as the other admin read surfaces.

export type ComplianceFramework = {
  id: string;
  name: string;
  description: string;
  created_by: string;
  created_at: string;
  updated_at: string;
};

export type CompliancePolicy = {
  id: string;
  name: string;
  description: string;
  enabled: boolean;
  mode: "inject" | "override";
  priority: number;
  applies_to_all: boolean;
  position_before: string;
  position_after: string;
  // framework_ids + config_yaml are populated by GET /{id}; the list endpoint
  // returns them empty (metadata only).
  framework_ids: string[];
  config_yaml: string;
  created_by: string;
  created_at: string;
  updated_at: string;
};

export async function listComplianceFrameworks(): Promise<ComplianceFramework[]> {
  return readJSON<ComplianceFramework[]>("/api/v1/admin/compliance/frameworks");
}

// listCompliancePolicies returns metadata only (framework_ids/config_yaml empty);
// fetch the full policy with getCompliancePolicy for editing.
export async function listCompliancePolicies(): Promise<CompliancePolicy[]> {
  return readJSON<CompliancePolicy[]>("/api/v1/admin/compliance/policies");
}

export async function getCompliancePolicy(id: string): Promise<CompliancePolicy> {
  return readJSON<CompliancePolicy>(
    `/api/v1/admin/compliance/policies/${encodeURIComponent(id)}`,
  );
}

// getProjectFrameworks lists the frameworks assigned to a project (by slug).
export async function getProjectFrameworks(
  slug: string,
): Promise<ComplianceFramework[]> {
  return readJSON<ComplianceFramework[]>(
    `/api/v1/admin/projects/${encodeURIComponent(slug)}/frameworks`,
  );
}

// PipelineDefView is the subset of a pipeline definition the preview renders.
// The control plane serialises the Go domain.Pipeline with capitalised keys
// (no JSON tags), so these match the wire shape exactly. Stages/Jobs are
// nullable because an empty definition marshals them as null.
export type PipelineDefView = {
  Stages: string[] | null;
  Jobs: { Name: string; Stage: string }[] | null;
};

// EffectivePipelinePreview is one pipeline's pre-policy (raw) and post-merge
// (effective) definition. system_managed flags the server-owned synthetic
// `_compliance` pipeline.
export type EffectivePipelinePreview = {
  name: string;
  system_managed: boolean;
  raw: PipelineDefView;
  effective: PipelineDefView;
};

// getEffectivePipelinePreview returns the stored effective definition for every
// pipeline of a project (what runs today) — a plain read, no recompute.
export async function getEffectivePipelinePreview(
  slug: string,
): Promise<EffectivePipelinePreview[]> {
  return readJSON<EffectivePipelinePreview[]>(
    `/api/v1/admin/projects/${encodeURIComponent(slug)}/effective-pipeline`,
  );
}
