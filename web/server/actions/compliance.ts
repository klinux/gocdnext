"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";
import type {
  ComplianceFramework,
  CompliancePolicy,
} from "@/server/queries/admin";

export type ActionResult = { ok: true } | { ok: false; error: string };
// Create returns the persisted DTO so the client can use the real id (and full
// row) immediately, instead of inventing an optimistic placeholder.
export type CreatedResult<T> = { ok: true; data: T } | { ok: false; error: string };

const frameworkSchema = z.object({
  name: z.string().min(1, "name is required"),
  description: z.string().default(""),
});

const policySchema = z.object({
  name: z.string().min(1, "name is required"),
  description: z.string().default(""),
  enabled: z.boolean().default(true),
  mode: z.enum(["inject", "override"]).default("inject"),
  priority: z.number().int().default(0),
  applies_to_all: z.boolean().default(false),
  position_before: z.string().default(""),
  position_after: z.string().default(""),
  framework_ids: z.array(z.string()).default([]),
  config_yaml: z.string().min(1, "config_yaml is required"),
});

const setFrameworksSchema = z.object({
  slug: z.string().min(1),
  framework_ids: z.array(z.string()).default([]),
});

async function apiFetch(path: string, init: RequestInit): Promise<Response> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  const session = (await cookies()).get("gocdnext_session")?.value;
  return fetch(url, {
    cache: "no-store",
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      ...(init.headers ?? {}),
    },
  });
}

function errorResult(res: Response, body: string): { ok: false; error: string } {
  return {
    ok: false,
    error: `server ${res.status}: ${body.trim().slice(0, 300) || "request failed"}`,
  };
}

function fail(err: unknown): { ok: false; error: string } {
  return { ok: false, error: err instanceof Error ? err.message : String(err) };
}

// ---- frameworks ----------------------------------------------------------

export async function createComplianceFramework(
  input: z.infer<typeof frameworkSchema>,
): Promise<CreatedResult<ComplianceFramework>> {
  const parsed = frameworkSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch("/api/v1/admin/compliance/frameworks", {
      method: "POST",
      body: JSON.stringify(parsed.data),
    });
    if (!res.ok) return errorResult(res, await res.text());
    const data = (await res.json()) as ComplianceFramework;
    revalidatePath("/admin/compliance");
    return { ok: true, data };
  } catch (err) {
    return fail(err);
  }
}

export async function updateComplianceFramework(
  input: z.infer<typeof frameworkSchema> & { id: string },
): Promise<ActionResult> {
  const parsed = frameworkSchema.extend({ id: z.string().min(1) }).safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const { id, ...body } = parsed.data;
  try {
    const res = await apiFetch(
      `/api/v1/admin/compliance/frameworks/${encodeURIComponent(id)}`,
      { method: "PUT", body: JSON.stringify(body) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/compliance");
    return { ok: true };
  } catch (err) {
    return fail(err);
  }
}

export async function deleteComplianceFramework(id: string): Promise<ActionResult> {
  try {
    const res = await apiFetch(
      `/api/v1/admin/compliance/frameworks/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/compliance");
    return { ok: true };
  } catch (err) {
    return fail(err);
  }
}

// ---- policies ------------------------------------------------------------

export async function createCompliancePolicy(
  input: z.infer<typeof policySchema>,
): Promise<CreatedResult<CompliancePolicy>> {
  const parsed = policySchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch("/api/v1/admin/compliance/policies", {
      method: "POST",
      body: JSON.stringify(parsed.data),
    });
    if (!res.ok) return errorResult(res, await res.text());
    const data = (await res.json()) as CompliancePolicy;
    revalidatePath("/admin/compliance");
    return { ok: true, data };
  } catch (err) {
    return fail(err);
  }
}

export async function updateCompliancePolicy(
  input: z.infer<typeof policySchema> & { id: string },
): Promise<ActionResult> {
  const parsed = policySchema.extend({ id: z.string().min(1) }).safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const { id, ...body } = parsed.data;
  try {
    const res = await apiFetch(
      `/api/v1/admin/compliance/policies/${encodeURIComponent(id)}`,
      { method: "PUT", body: JSON.stringify(body) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/compliance");
    return { ok: true };
  } catch (err) {
    return fail(err);
  }
}

export async function deleteCompliancePolicy(id: string): Promise<ActionResult> {
  try {
    const res = await apiFetch(
      `/api/v1/admin/compliance/policies/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/compliance");
    return { ok: true };
  } catch (err) {
    return fail(err);
  }
}

// ---- per-project framework assignment ------------------------------------

export async function setProjectFrameworks(
  input: z.infer<typeof setFrameworksSchema>,
): Promise<ActionResult> {
  const parsed = setFrameworksSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const { slug, framework_ids } = parsed.data;
  try {
    const res = await apiFetch(
      `/api/v1/admin/projects/${encodeURIComponent(slug)}/frameworks`,
      { method: "PUT", body: JSON.stringify({ framework_ids }) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath(`/projects/${slug}/settings`);
    return { ok: true };
  } catch (err) {
    return fail(err);
  }
}
