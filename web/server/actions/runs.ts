"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";
import { getRunDetail, GocdnextAPIError } from "@/server/queries/projects";
import type { JobDetail, RunDetail } from "@/types/api";

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

const jobDetailSchema = z.object({
  runId: uuidSchema,
  jobId: uuidSchema,
  logLines: z.number().int().min(0).max(500).optional(),
});

export type JobDetailResult =
  | {
      ok: true;
      job: JobDetail;
      run: Pick<
        RunDetail,
        "id" | "counter" | "status" | "pipeline_name" | "project_slug"
      >;
      stageName: string;
    }
  | { ok: false; error: string; status?: number };

// Drawer-oriented fetch: we pluck the requested job from the full
// run detail. Logs are capped small so the drawer pops fast — the
// full run page is linked for the deep-dive view.
export async function fetchJobDetail(
  input: z.infer<typeof jobDetailSchema>,
): Promise<JobDetailResult> {
  const parsed = jobDetailSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const detail = await getRunDetail(parsed.data.runId, parsed.data.logLines ?? 50);
    for (const stage of detail.stages) {
      const job = stage.jobs.find((j) => j.id === parsed.data.jobId);
      if (!job) continue;
      return {
        ok: true,
        job,
        stageName: stage.name,
        run: {
          id: detail.id,
          counter: detail.counter,
          status: detail.status,
          pipeline_name: detail.pipeline_name,
          project_slug: detail.project_slug,
        },
      };
    }
    return { ok: false, error: "job not found in this run", status: 404 };
  } catch (err) {
    if (err instanceof GocdnextAPIError) {
      return { ok: false, error: err.message, status: err.status };
    }
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
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
