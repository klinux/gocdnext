"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";
import { deployTargetFormSchema } from "@/lib/deploy-target";

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

const setDeployTargetSchema = deployTargetFormSchema.extend({
  slug: z.string().min(1),
});

// setDeployTarget registers or updates the native deploy target (ADR-0001)
// for an environment — a maintainer-gated upsert keyed 1:1 on the environment
// name. `provider` is never sent; the server hardcodes "argocd". The server
// re-validates every field AND fetches the ArgoCD Application (existence,
// reachability, single-source) before writing, so a 4xx here carries a
// user-ready message (e.g. a multi-source rejection, or "check the cluster is
// reachable and the application exists") which we surface verbatim.
export async function setDeployTarget(
  input: z.infer<typeof setDeployTargetSchema>,
): Promise<ActionResult> {
  const parsed = setDeployTargetSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  const {
    slug,
    environment,
    cluster,
    application,
    namespace,
    sync_mode,
    rollout_aware,
    rollout_cluster,
    rollout_namespace,
    rollout_name,
  } = parsed.data;
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(slug)}/deploy-targets`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      // Omit empties so the server applies its defaults (namespace→argocd, rollout
      // routing→App cluster / auto-discover). Rollout routing is only sent when
      // rollout_aware — the server also drops it otherwise, this keeps the wire clean.
      body: JSON.stringify({
        environment,
        cluster,
        application,
        sync_mode,
        ...(namespace ? { namespace } : {}),
        rollout_aware: rollout_aware ?? false,
        ...(rollout_aware && rollout_cluster ? { rollout_cluster } : {}),
        ...(rollout_aware && rollout_namespace ? { rollout_namespace } : {}),
        ...(rollout_aware && rollout_name ? { rollout_name } : {}),
      }),
    });
    if (!res.ok) {
      // The control plane writes human-readable, safe messages per status for
      // this endpoint, so surface the body directly rather than a code.
      const body = (await res.text()).trim();
      return { ok: false, error: body || `server error (${res.status})` };
    }
    revalidatePath(`/projects/${slug}/environments`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

const deleteDeployTargetSchema = z.object({
  slug: z.string().min(1),
  environment: z.string().min(1),
});

// deleteDeployTarget removes an environment's native target, reverting it to a
// tracking-layer deploy (the pipeline's own script/plugin runs again). 404
// ("deploy target not found") is surfaced as an error so a double-delete is
// visible rather than silently "succeeding".
export async function deleteDeployTarget(
  input: z.infer<typeof deleteDeployTargetSchema>,
): Promise<ActionResult> {
  const parsed = deleteDeployTargetSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  const { slug, environment } = parsed.data;
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(slug)}/deploy-targets/${encodeURIComponent(environment)}`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "DELETE",
      cache: "no-store",
      headers: {
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
    });
    if (!res.ok) {
      const body = (await res.text()).trim();
      return { ok: false, error: body || `server error (${res.status})` };
    }
    revalidatePath(`/projects/${slug}/environments`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
