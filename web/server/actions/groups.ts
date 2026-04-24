"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";
import { env } from "@/lib/env";

const nameSchema = z
  .string()
  .min(1, "name is required")
  .regex(/^[A-Za-z0-9._-]+$/, "name: letters, digits, dash, underscore, dot only");

const writeSchema = z.object({
  name: nameSchema,
  description: z.string().optional().default(""),
});

const updateSchema = writeSchema.extend({ id: z.string().min(1) });
const deleteSchema = z.object({ id: z.string().min(1) });
const memberSchema = z.object({
  groupID: z.string().min(1),
  userID: z.string().min(1),
});

export type ActionResult = { ok: true } | { ok: false; error: string };

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

export async function createGroup(
  input: z.infer<typeof writeSchema>,
): Promise<ActionResult> {
  const parsed = writeSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch("/api/v1/admin/groups", {
      method: "POST",
      body: JSON.stringify(parsed.data),
    });
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/groups");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function updateGroup(
  input: z.infer<typeof updateSchema>,
): Promise<ActionResult> {
  const parsed = updateSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const { id, ...body } = parsed.data;
    const res = await apiFetch(
      `/api/v1/admin/groups/${encodeURIComponent(id)}`,
      { method: "PUT", body: JSON.stringify(body) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/groups");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function deleteGroup(
  input: z.infer<typeof deleteSchema>,
): Promise<ActionResult> {
  const parsed = deleteSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch(
      `/api/v1/admin/groups/${encodeURIComponent(parsed.data.id)}`,
      { method: "DELETE" },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/groups");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function addGroupMember(
  input: z.infer<typeof memberSchema>,
): Promise<ActionResult> {
  const parsed = memberSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch(
      `/api/v1/admin/groups/${encodeURIComponent(parsed.data.groupID)}/members`,
      { method: "POST", body: JSON.stringify({ user_id: parsed.data.userID }) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath(`/admin/groups/${parsed.data.groupID}`);
    revalidatePath("/admin/groups");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function removeGroupMember(
  input: z.infer<typeof memberSchema>,
): Promise<ActionResult> {
  const parsed = memberSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch(
      `/api/v1/admin/groups/${encodeURIComponent(parsed.data.groupID)}/members/${encodeURIComponent(parsed.data.userID)}`,
      { method: "DELETE" },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath(`/admin/groups/${parsed.data.groupID}`);
    revalidatePath("/admin/groups");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
