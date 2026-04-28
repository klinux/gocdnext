"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

export type ActionResult<T = void> =
  | { ok: true; data: T }
  | { ok: false; error: string };

async function call<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<ActionResult<T>> {
  try {
    const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method,
      cache: "no-store",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
    if (!res.ok) {
      const text = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${text.trim().slice(0, 300)}`,
      };
    }
    if (res.status === 204) return { ok: true, data: undefined as T };
    const data = (await res.json()) as T;
    return { ok: true, data };
  } catch (err) {
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}

// ---- per-user ----------------------------------------------------

const createUserTokenSchema = z.object({
  name: z.string().min(1).max(120),
  // ISO timestamp; empty → null (no expiry)
  expires_at: z.string().optional().nullable(),
});

export type CreateTokenResponse = {
  token: {
    id: string;
    name: string;
    prefix: string;
    expires_at?: string | null;
    last_used_at?: string | null;
    revoked_at?: string | null;
    created_at: string;
  };
  plaintext: string;
};

export async function createUserAPIToken(
  input: z.infer<typeof createUserTokenSchema>,
): Promise<ActionResult<CreateTokenResponse>> {
  const parsed = createUserTokenSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid" };
  }
  const res = await call<CreateTokenResponse>(
    "POST",
    "/api/v1/account/api-tokens",
    {
      name: parsed.data.name,
      expires_at: parsed.data.expires_at || null,
    },
  );
  if (res.ok) revalidatePath("/account");
  return res;
}

export async function revokeUserAPIToken(
  id: string,
): Promise<ActionResult<void>> {
  const res = await call<void>(
    "DELETE",
    `/api/v1/account/api-tokens/${encodeURIComponent(id)}`,
  );
  if (res.ok) revalidatePath("/account");
  return res;
}

// ---- service accounts (admin) -----------------------------------

const createSASchema = z.object({
  name: z.string().min(1).max(120),
  description: z.string().max(1000).optional().default(""),
  role: z.enum(["admin", "maintainer", "viewer"]),
});

export async function createServiceAccount(
  input: z.infer<typeof createSASchema>,
): Promise<ActionResult<{ id: string }>> {
  const parsed = createSASchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid" };
  }
  const res = await call<{ id: string }>(
    "POST",
    "/api/v1/admin/service-accounts",
    parsed.data,
  );
  if (res.ok) revalidatePath("/admin/service-accounts");
  return res;
}

const updateSASchema = z.object({
  id: z.string().uuid(),
  description: z.string().max(1000).optional().default(""),
  role: z.enum(["admin", "maintainer", "viewer"]),
});

export async function updateServiceAccount(
  input: z.infer<typeof updateSASchema>,
): Promise<ActionResult<void>> {
  const parsed = updateSASchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid" };
  }
  const res = await call<void>(
    "PUT",
    `/api/v1/admin/service-accounts/${encodeURIComponent(parsed.data.id)}`,
    { description: parsed.data.description, role: parsed.data.role },
  );
  if (res.ok) revalidatePath("/admin/service-accounts");
  return res;
}

export async function setServiceAccountDisabled(
  id: string,
  disabled: boolean,
): Promise<ActionResult<void>> {
  const res = await call<void>(
    "POST",
    `/api/v1/admin/service-accounts/${encodeURIComponent(id)}/disable`,
    { disabled },
  );
  if (res.ok) revalidatePath("/admin/service-accounts");
  return res;
}

export async function deleteServiceAccount(
  id: string,
): Promise<ActionResult<void>> {
  const res = await call<void>(
    "DELETE",
    `/api/v1/admin/service-accounts/${encodeURIComponent(id)}`,
  );
  if (res.ok) revalidatePath("/admin/service-accounts");
  return res;
}

const createSATokenSchema = z.object({
  saID: z.string().uuid(),
  name: z.string().min(1).max(120),
  expires_at: z.string().optional().nullable(),
});

export async function createSAToken(
  input: z.infer<typeof createSATokenSchema>,
): Promise<ActionResult<CreateTokenResponse>> {
  const parsed = createSATokenSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid" };
  }
  const res = await call<CreateTokenResponse>(
    "POST",
    `/api/v1/admin/service-accounts/${encodeURIComponent(parsed.data.saID)}/tokens`,
    { name: parsed.data.name, expires_at: parsed.data.expires_at || null },
  );
  if (res.ok) revalidatePath("/admin/service-accounts");
  return res;
}

export async function revokeSAToken(
  saID: string,
  tokenID: string,
): Promise<ActionResult<void>> {
  const res = await call<void>(
    "DELETE",
    `/api/v1/admin/service-accounts/${encodeURIComponent(saID)}/tokens/${encodeURIComponent(tokenID)}`,
  );
  if (res.ok) revalidatePath("/admin/service-accounts");
  return res;
}
