"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

const decisionSchema = z.object({
  jobRunID: z.string().min(1),
  runID: z.string().min(1), // for revalidation only
});

export type ActionResult = { ok: true } | { ok: false; error: string };

export async function approveJob(
  input: z.infer<typeof decisionSchema>,
): Promise<ActionResult> {
  return decide(input, "approve");
}

export async function rejectJob(
  input: z.infer<typeof decisionSchema>,
): Promise<ActionResult> {
  return decide(input, "reject");
}

async function decide(
  input: z.infer<typeof decisionSchema>,
  verb: "approve" | "reject",
): Promise<ActionResult> {
  const parsed = decisionSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/job_runs/${encodeURIComponent(parsed.data.jobRunID)}/${verb}`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: session ? { Cookie: `gocdnext_session=${session}` } : {},
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 200) || `${verb} failed`}`,
      };
    }
    // Run page shows the gate — refresh it so the new status /
    // decided_by / finished_at render without a hard reload.
    revalidatePath(`/runs/${parsed.data.runID}`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: errorMessage(err) };
  }
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
