"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

// Zod schemas mirror the backend. Empty private_key /
// webhook_secret are allowed on update — the store preserves the
// existing ciphertext via COALESCE. The UI form renders stored
// secrets as "••••" and sends "" when the admin doesn't retype.

const upsertSchema = z.object({
  name: z
    .string()
    .min(1)
    .max(64)
    .regex(/^[a-z][a-z0-9-]*$/, "lowercase, digits, dashes only"),
  kind: z.enum(["github_app"]),
  display_name: z.string().max(128).optional(),
  app_id: z.number().int().positive(),
  private_key: z.string().max(16 * 1024).optional(),
  webhook_secret: z.string().max(512).optional(),
  api_base: z.string().max(512).optional(),
  enabled: z.boolean(),
});

export type VCSActionResult =
  | { ok: true; data: Record<string, unknown> }
  | { ok: false; error: string; status?: number };

export async function upsertVCSIntegration(
  input: z.infer<typeof upsertSchema>,
): Promise<VCSActionResult> {
  const parsed = upsertSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const res = await forwardJSON("POST", "/api/v1/admin/integrations/vcs", {
    ...parsed.data,
    private_key: parsed.data.private_key ?? "",
    webhook_secret: parsed.data.webhook_secret ?? "",
    api_base: parsed.data.api_base ?? "",
    display_name: parsed.data.display_name ?? "",
  });
  if (res.ok) {
    revalidatePath("/settings/integrations");
  }
  return res;
}

export async function deleteVCSIntegration(input: {
  id: string;
}): Promise<VCSActionResult> {
  const parse = z.object({ id: z.string().uuid() }).safeParse(input);
  if (!parse.success) {
    return { ok: false, error: parse.error.issues[0]?.message ?? "invalid id" };
  }
  const res = await forwardJSON(
    "DELETE",
    `/api/v1/admin/integrations/vcs/${parse.data.id}`,
  );
  if (res.ok) {
    revalidatePath("/settings/integrations");
  }
  return res;
}

export async function reloadVCSIntegrations(): Promise<VCSActionResult> {
  const res = await forwardJSON(
    "POST",
    "/api/v1/admin/integrations/vcs/reload",
  );
  if (res.ok) {
    revalidatePath("/settings/integrations");
  }
  return res;
}

async function forwardJSON(
  method: string,
  path: string,
  body?: unknown,
): Promise<VCSActionResult> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  const store = await cookies();
  const session = store.get("gocdnext_session")?.value;
  try {
    const res = await fetch(url, {
      method,
      cache: "no-store",
      headers: {
        Accept: "application/json",
        ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    if (!res.ok) {
      return {
        ok: false,
        status: res.status,
        error: text.trim().slice(0, 300) || `server ${res.status}`,
      };
    }
    const data = text ? (JSON.parse(text) as Record<string, unknown>) : {};
    return { ok: true, data };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
