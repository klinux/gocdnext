"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

const SESSION_COOKIE = "gocdnext_session";

// Matches the server-side CHECK constraint.
const kindSchema = z.enum(["github", "oidc"]);

// Empty client_secret is allowed on update (the store layer
// preserves the existing ciphertext). Other fields get the usual
// "not an empty string" guardrail at this layer — the handler
// re-validates anyway.
const upsertSchema = z.object({
  name: z.string().min(1).max(64).regex(/^[a-z][a-z0-9_-]*$/, "lowercase, digits, -, _ only"),
  kind: kindSchema,
  display_name: z.string().max(64).optional(),
  client_id: z.string().min(1).max(256),
  client_secret: z.string().max(512).optional(),
  issuer: z.string().max(512).optional(),
  github_api_base: z.string().max(512).optional(),
  enabled: z.boolean(),
});

export type AuthProviderResult =
  | { ok: true; data: Record<string, unknown> }
  | { ok: false; error: string; status?: number };

export async function upsertAuthProvider(
  input: z.infer<typeof upsertSchema>,
): Promise<AuthProviderResult> {
  const parsed = upsertSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const res = await forwardJSON(
    "POST",
    "/api/v1/admin/auth/providers",
    parsed.data,
  );
  if (res.ok) {
    revalidatePath("/settings/auth");
  }
  return res;
}

export async function deleteAuthProvider(input: {
  id: string;
}): Promise<AuthProviderResult> {
  const parse = z.object({ id: z.string().uuid() }).safeParse(input);
  if (!parse.success) {
    return { ok: false, error: parse.error.issues[0]?.message ?? "invalid id" };
  }
  const res = await forwardJSON(
    "DELETE",
    `/api/v1/admin/auth/providers/${parse.data.id}`,
  );
  if (res.ok) {
    revalidatePath("/settings/auth");
  }
  return res;
}

export async function reloadAuthProviders(): Promise<AuthProviderResult> {
  const res = await forwardJSON("POST", "/api/v1/admin/auth/providers/reload");
  if (res.ok) {
    revalidatePath("/settings/auth");
  }
  return res;
}

async function forwardJSON(
  method: string,
  path: string,
  body?: unknown,
): Promise<AuthProviderResult> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  const store = await cookies();
  const session = store.get(SESSION_COOKIE)?.value;
  try {
    const res = await fetch(url, {
      method,
      cache: "no-store",
      headers: {
        Accept: "application/json",
        ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
        ...(session ? { Cookie: `${SESSION_COOKIE}=${session}` } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    if (!res.ok) {
      return {
        ok: false,
        status: res.status,
        error: text.trim().slice(0, 200) || `server ${res.status}`,
      };
    }
    const data = text ? (JSON.parse(text) as Record<string, unknown>) : {};
    return { ok: true, data };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
