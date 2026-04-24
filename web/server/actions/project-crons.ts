"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";
import { env } from "@/lib/env";

// cronExpressionSchema does a lightweight structural check — the
// server re-validates with the robfig parser (authoritative). We
// just reject empty/whitespace here so the form can say "required"
// without a round-trip.
const cronExpressionSchema = z
  .string()
  .min(1, "expression is required")
  .refine((s) => s.trim() !== "", "expression is required");

const writeSchema = z.object({
  slug: z.string().min(1),
  name: z.string().min(1, "name is required"),
  expression: cronExpressionSchema,
  pipeline_ids: z.array(z.string()),
  enabled: z.boolean(),
});

const updateSchema = writeSchema.extend({
  id: z.string().min(1),
});

const deleteSchema = z.object({
  slug: z.string().min(1),
  id: z.string().min(1),
});

const runAllSchema = z.object({
  slug: z.string().min(1),
});

export type ActionResult = { ok: true } | { ok: false; error: string };

export type RunAllResult = {
  ok: true;
  results: Array<{ pipeline_id: string; run_id?: string; error?: string }>;
} | {
  ok: false;
  error: string;
};

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

export async function createProjectCron(
  input: z.infer<typeof writeSchema>,
): Promise<ActionResult> {
  const parsed = writeSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const { slug, ...body } = parsed.data;
    const res = await apiFetch(
      `/api/v1/projects/${encodeURIComponent(slug)}/crons`,
      { method: "POST", body: JSON.stringify(body) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath(`/projects/${slug}/crons`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function updateProjectCron(
  input: z.infer<typeof updateSchema>,
): Promise<ActionResult> {
  const parsed = updateSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const { slug, id, ...body } = parsed.data;
    const res = await apiFetch(
      `/api/v1/projects/${encodeURIComponent(slug)}/crons/${encodeURIComponent(id)}`,
      { method: "PUT", body: JSON.stringify(body) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath(`/projects/${slug}/crons`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function deleteProjectCron(
  input: z.infer<typeof deleteSchema>,
): Promise<ActionResult> {
  const parsed = deleteSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const { slug, id } = parsed.data;
    const res = await apiFetch(
      `/api/v1/projects/${encodeURIComponent(slug)}/crons/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath(`/projects/${slug}/crons`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function runAllPipelines(
  input: z.infer<typeof runAllSchema>,
): Promise<RunAllResult> {
  const parsed = runAllSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch(
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/run-all`,
      { method: "POST" },
    );
    if (!res.ok) return errorResult(res, await res.text());
    const body = (await res.json()) as {
      results: Array<{ pipeline_id: string; run_id?: string; error?: string }>;
    };
    revalidatePath(`/projects/${parsed.data.slug}`);
    return { ok: true, results: body.results };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
