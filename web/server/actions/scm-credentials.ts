"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";
import { env } from "@/lib/env";

const writeSchema = z.object({
  provider: z.enum(["gitlab", "bitbucket"]),
  host: z.string().min(1, "host is required").max(253),
  api_base: z.string().max(512).optional(),
  display_name: z.string().max(128).optional(),
  auth_ref: z.string().min(1, "auth_ref is required").max(4096),
});

const deleteSchema = z.object({
  id: z.string().min(1),
});

export type ActionResult = { ok: true } | { ok: false; error: string };

async function apiFetch(
  path: string,
  init: RequestInit,
): Promise<Response> {
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

export async function upsertSCMCredential(
  input: z.infer<typeof writeSchema>,
): Promise<ActionResult> {
  const parsed = writeSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const res = await apiFetch("/api/v1/admin/scm-credentials", {
      method: "POST",
      body: JSON.stringify(parsed.data),
    });
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/settings/integrations");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function deleteSCMCredential(
  input: z.infer<typeof deleteSchema>,
): Promise<ActionResult> {
  const parsed = deleteSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const res = await apiFetch(
      `/api/v1/admin/scm-credentials/${encodeURIComponent(parsed.data.id)}`,
      { method: "DELETE" },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/settings/integrations");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
