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

const redeployCurrentSchema = z.object({
  slug: z.string().min(1),
  environmentId: z.string().min(1),
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

// redeployCurrentEnvironment re-runs the environment's current successful
// deploy as a normal deploy. The server resolves "current" at request time and
// refuses frozen/no-current/GC'd-run cases.
export async function redeployCurrentEnvironment(
  input: z.infer<typeof redeployCurrentSchema>,
): Promise<ActionResult> {
  const parsed = redeployCurrentSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/environments/${encodeURIComponent(parsed.data.environmentId)}/redeploy`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 200) || "redeploy failed"}`,
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
    governing_gate,
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
        // Round-trip the gate verbatim: the server reads an ABSENT governing_gate as
        // "remove it" (admin-only), so an edit that isn't touching the gate must send
        // it back unchanged, else a maintainer editing a non-gate field would 403.
        ...(governing_gate ? { governing_gate } : {}),
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

const deleteEnvironmentSchema = z.object({
  slug: z.string().min(1),
  environmentId: z.string().min(1),
});

// deleteEnvironment hard-deletes an environment and everything under it — the
// server cascades its whole deploy history + any registered target, and gates
// this to admin. Environments are lazy, so a later deploy to the same name
// re-creates it empty. The 403/404 bodies are surfaced verbatim so a
// non-admin / double-delete is visible rather than silently "succeeding".
export async function deleteEnvironment(
  input: z.infer<typeof deleteEnvironmentSchema>,
): Promise<ActionResult> {
  const parsed = deleteEnvironmentSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  const { slug, environmentId } = parsed.data;
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(slug)}/environments/${encodeURIComponent(environmentId)}`;
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

// The environment-name grammar the server enforces (server/pkg/domain:
// deployEnvRE). Validated client-side purely for a fast, specific message — the
// server re-validates and stays the authority.
const environmentNameSchema = z
  .string()
  .min(1, "environment name is required")
  .max(64, "environment name is at most 64 characters")
  .regex(
    /^[a-zA-Z0-9][a-zA-Z0-9._-]*$/,
    "start with a letter or digit, then letters, digits, dot, dash or underscore",
  );

const freezeEnvironmentSchema = z.object({
  slug: z.string().min(1),
  name: environmentNameSchema,
  reason: z
    .string()
    .trim()
    .min(1, "a reason is required")
    .max(500, "reason is at most 500 characters"),
});

// freezeEnvironment puts a deploy environment under a system-enforced
// change-freeze (#202): while it holds, gocdnext admits no promotion to that
// environment — approving a gate that governs it is refused, its deploy jobs
// stay queued, and rollback is refused.
//
// Keyed by NAME rather than environment id on purpose: environments are created
// lazily at their first deploy, so this is also how you pre-emptively freeze one
// that has never been deployed to — the first-ever deploy is exactly the one a
// freeze must stop.
//
// Idempotent server-side: re-freezing does NOT reset who froze it, when, or why.
export async function freezeEnvironment(
  input: z.infer<typeof freezeEnvironmentSchema>,
): Promise<ActionResult> {
  const parsed = freezeEnvironmentSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const { slug, name, reason } = parsed.data;
  return callFreezeEndpoint(slug, name, "PUT", { reason });
}

const unfreezeEnvironmentSchema = z.object({
  slug: z.string().min(1),
  name: environmentNameSchema,
});

// unfreezeEnvironment lifts the change-freeze. The server wakes every run the
// freeze was holding, so deploys resume immediately rather than on the next
// scheduler tick. Idempotent.
export async function unfreezeEnvironment(
  input: z.infer<typeof unfreezeEnvironmentSchema>,
): Promise<ActionResult> {
  const parsed = unfreezeEnvironmentSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const { slug, name } = parsed.data;
  return callFreezeEndpoint(slug, name, "DELETE");
}

async function callFreezeEndpoint(
  slug: string,
  name: string,
  method: "PUT" | "DELETE",
  body?: { reason: string },
): Promise<ActionResult> {
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(slug)}/environment-freezes/${encodeURIComponent(name)}`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method,
      cache: "no-store",
      headers: {
        ...(body ? { "Content-Type": "application/json" } : {}),
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    if (!res.ok) {
      // The handler writes user-ready messages per status (invalid name,
      // missing reason, not a maintainer), so surface the body verbatim.
      const text = (await res.text()).trim();
      return { ok: false, error: text || `server error (${res.status})` };
    }
    revalidatePath(`/projects/${slug}/environments`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

const rolloutGateSchema = z.object({
  slug: z.string().min(1),
  revisionId: z.string().min(1),
  // The armed gate token, echoed so a stale tab voting on a superseded step gets a 409.
  gateId: z.string().min(1),
});

// approveRolloutGate / rejectRolloutGate vote on an armed canary gate (ADR-0001 Phase 2).
// Approve promotes the paused canary one step once quorum is met; reject ABORTS the
// rollout (traffic → stable — not a Git revert). The server enforces the approvers
// allow-list + the gate_id token; a 4xx body is a user-ready message we surface verbatim.
export async function approveRolloutGate(
  input: z.infer<typeof rolloutGateSchema>,
): Promise<ActionResult> {
  return decideRolloutGate(input, "approve");
}

export async function rejectRolloutGate(
  input: z.infer<typeof rolloutGateSchema>,
): Promise<ActionResult> {
  return decideRolloutGate(input, "reject");
}

async function decideRolloutGate(
  input: z.infer<typeof rolloutGateSchema>,
  verb: "approve" | "reject",
): Promise<ActionResult> {
  const parsed = rolloutGateSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const { slug, revisionId, gateId } = parsed.data;
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(slug)}/deploy-watches/${encodeURIComponent(revisionId)}/${verb}`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ gate_id: gateId }),
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

const rolloutActionSchema = z.object({
  slug: z.string().min(1),
  cluster: z.string().min(1),
  namespace: z.string().min(1),
  name: z.string().min(1),
});

export type RolloutActionInput = z.infer<typeof rolloutActionSchema>;

// promoteRollout / abortRollout directly actuate a NON-gated Argo Rollouts canary from the
// rollouts dashboard (ADR-0001, PR-C). Promote advances the paused canary one step; abort
// shifts traffic back to the stable version (NOT a Git revert). The server is the
// authority: a GATED rollout is refused with 409 (its decision must go through the
// Approve/Reject vote path) and an unreachable cluster / missing Rollout is a 404 — both
// carry a user-ready body we surface verbatim.
export async function promoteRollout(
  input: RolloutActionInput,
): Promise<ActionResult> {
  return actuateRollout(input, "promote");
}

export async function abortRollout(
  input: RolloutActionInput,
): Promise<ActionResult> {
  return actuateRollout(input, "abort");
}

async function actuateRollout(
  input: RolloutActionInput,
  verb: "promote" | "abort",
): Promise<ActionResult> {
  const parsed = rolloutActionSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const { slug, cluster, namespace, name } = parsed.data;
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(slug)}/rollouts/${encodeURIComponent(cluster)}/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/${verb}`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
    });
    if (!res.ok) {
      const body = (await res.text()).trim();
      return { ok: false, error: body || `server error (${res.status})` };
    }
    // The rollouts page is client-polled (react-query); the client invalidates its query
    // for an immediate refresh. Revalidate the RSC shell too so a full navigation is fresh.
    revalidatePath(`/projects/${slug}/rollouts`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
