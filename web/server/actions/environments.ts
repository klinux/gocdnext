"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

const rollbackSchema = z.object({
  slug: z.string().min(1),
  environmentId: z.string().min(1),
  // Shape isn't enforced client-side — the API returns 400 on a
  // malformed id so the handler stays the one source of truth.
  toRevisionId: z.string().min(1),
});

export type ActionResult = { ok: true } | { ok: false; error: string };

// rollbackEnvironment re-runs the deploy job of a past revision (#39
// phase 3). The API returns 202 — the re-dispatch is async — so a
// successful result means "rollback started", not "rolled back". The
// Environments tab is revalidated so the run reopening and (eventually)
// the new current version surface on the next render.
export async function rollbackEnvironment(
  input: z.infer<typeof rollbackSchema>,
): Promise<ActionResult> {
  const parsed = rollbackSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/environments/${encodeURIComponent(parsed.data.environmentId)}/rollback`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ to_revision_id: parsed.data.toRevisionId }),
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 200) || "rollback failed"}`,
      };
    }
    revalidatePath(`/projects/${parsed.data.slug}/environments`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
