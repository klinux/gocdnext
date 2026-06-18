"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";
import { clusterAuthTypes } from "@/lib/clusters";

// A "use server" module may export only async functions — Next strips
// any const/type export, breaking client imports. So the auth-type
// enum + the preserve sentinel + the input types live in @/lib/clusters
// and are imported here (and by the client manager) from there.

// name: lowercase DNS-ish — first char alnum, then alnum/_/-, up to 63
// total. Matches the server's identifier rule so a typo fails the same
// way locally instead of after the round-trip.
const nameSchema = z
  .string()
  .min(1, "name is required")
  .regex(
    /^[a-z0-9][a-z0-9_-]{0,62}$/,
    "name: lowercase, start alnum, then letters/digits/dash/underscore (max 63)",
  );

const writeSchema = z.object({
  name: nameSchema,
  description: z.string().optional().default(""),
  auth_type: z.enum(clusterAuthTypes),
  // api_server is only meaningful for the token flow; kept optional so
  // kubeconfig / in_cluster can submit it empty. The server cross-checks
  // it against auth_type.
  api_server: z.string().optional().default(""),
  // ca_cert / credential are write-only secrets — optional here because
  // an edit that preserves the stored credential sends the sentinel
  // (not a fresh value) and in_cluster needs neither.
  ca_cert: z.string().optional().default(""),
  credential: z.string().optional().default(""),
  // Project IDs allowed to target this cluster. Empty = no allow-list
  // entries (the server decides whether that means "all" or "none").
  allowed_projects: z.array(z.string().min(1)).optional().default([]),
});

const updateSchema = writeSchema.extend({ id: z.string().min(1) });
const deleteSchema = z.object({ id: z.string().min(1) });

export type ClusterWriteInput = z.infer<typeof writeSchema>;
export type ClusterUpdateInput = z.infer<typeof updateSchema>;
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

export async function createCluster(
  input: ClusterWriteInput,
): Promise<ActionResult> {
  const parsed = writeSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch("/api/v1/admin/clusters", {
      method: "POST",
      body: JSON.stringify(parsed.data),
    });
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/clusters");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function updateCluster(
  input: ClusterUpdateInput,
): Promise<ActionResult> {
  const parsed = updateSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const { id, ...body } = parsed.data;
    const res = await apiFetch(
      `/api/v1/admin/clusters/${encodeURIComponent(id)}`,
      { method: "PUT", body: JSON.stringify(body) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/clusters");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function deleteCluster(
  input: z.infer<typeof deleteSchema>,
): Promise<ActionResult> {
  const parsed = deleteSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch(
      `/api/v1/admin/clusters/${encodeURIComponent(parsed.data.id)}`,
      { method: "DELETE" },
    );
    // 409 (cluster still referenced) carries a human message from the
    // server — surface it verbatim so the operator knows what to detach.
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/clusters");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
