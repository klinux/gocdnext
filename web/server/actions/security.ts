"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

const setStateSchema = z.object({
  slug: z.string().min(1),
  stateId: z.number().int().positive(),
  state: z.enum(["open", "dismissed", "false_positive", "accepted"]),
  reason: z.string().max(1000).optional(),
});

export type ActionResult = { ok: true } | { ok: false; error: string };

// setFindingState PUTs the control plane's finding-state endpoint. RBAC
// (maintainer+) is enforced server-side; a 403 surfaces as a user-facing error
// the caller toasts. State persists by identity, so a dismiss sticks across
// future scans.
export async function setFindingState(
  input: z.infer<typeof setStateSchema>,
): Promise<ActionResult> {
  const parsed = setStateSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const { slug, stateId, state, reason } = parsed.data;
  const url =
    env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
    `/api/v1/projects/${encodeURIComponent(slug)}/finding-states/${stateId}/state`;
  try {
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "PUT",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ state, reason: reason ?? "" }),
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 300) || "request failed"}`,
      };
    }
    revalidatePath(`/projects/${slug}/security`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
