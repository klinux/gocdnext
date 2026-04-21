"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

// All three action endpoints share the same wire shape on the client
// side: either { ok: true, data } with whatever the server returned,
// or { ok: false, error } with a string suitable for a toast. The
// pages catch unknown errors here so Next.js doesn't render a full
// error boundary for a failed POST.

const uuidSchema = z.string().uuid({ message: "expected UUID" });

const cancelSchema = z.object({ runId: uuidSchema });
const rerunSchema = z.object({ runId: uuidSchema, triggeredBy: z.string().optional() });
const triggerSchema = z.object({
  pipelineId: uuidSchema,
  projectSlug: z.string().min(1),
  triggeredBy: z.string().optional(),
});

export type RunActionResult =
  | { ok: true; data: Record<string, unknown> }
  | { ok: false; error: string; status?: number };

export async function cancelRun(
  input: z.infer<typeof cancelSchema>,
): Promise<RunActionResult> {
  const parsed = cancelSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const res = await postJSON(`/api/v1/runs/${parsed.data.runId}/cancel`, {});
  if (res.ok) {
    revalidatePath(`/runs/${parsed.data.runId}`);
    revalidatePath("/runs");
    revalidatePath("/");
  }
  return res;
}

export async function rerunRun(
  input: z.infer<typeof rerunSchema>,
): Promise<RunActionResult> {
  const parsed = rerunSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const res = await postJSON(`/api/v1/runs/${parsed.data.runId}/rerun`, {
    triggered_by: parsed.data.triggeredBy ?? "",
  });
  if (res.ok) {
    revalidatePath("/runs");
    revalidatePath("/");
  }
  return res;
}

export async function triggerPipelineRun(
  input: z.infer<typeof triggerSchema>,
): Promise<RunActionResult> {
  const parsed = triggerSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const res = await postJSON(`/api/v1/pipelines/${parsed.data.pipelineId}/trigger`, {
    triggered_by: parsed.data.triggeredBy ?? "",
  });
  if (res.ok) {
    revalidatePath(`/projects/${parsed.data.projectSlug}`);
    revalidatePath("/runs");
    revalidatePath("/");
  }
  return res;
}

async function postJSON(path: string, body: unknown): Promise<RunActionResult> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  // Forward the session cookie so the backend's RequireAuth
  // middleware sees the logged-in user. Server actions run on
  // the Node side and don't inherit browser cookies automatically
  // — omitting this is how the handler returned "not authenticated"
  // on trigger/cancel/rerun calls.
  const store = await cookies();
  const session = store.get("gocdnext_session")?.value;
  try {
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify(body),
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
