"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

// k8s quantity format: optional sign + number (decimal allowed) +
// optional suffix. We don't try to enforce the full grammar — the
// server validates against the same lib k8s uses; this regex is a
// quick "obvious typo" gate before the round-trip.
const quantitySchema = z
  .string()
  .regex(/^$|^\d+(\.\d+)?[a-zA-Z]*$/, "must be a k8s quantity (e.g. 100m, 256Mi)");

const tagSchema = z.string().regex(/^[A-Za-z0-9._-]+$/, "tag: letters, digits, dash, underscore, dot only");

// env / secret keys mirror the conventional UPPER_SNAKE shape that
// shells, Docker and Kubernetes all converge on. Mirrors the
// validEnvKey check on the server so a typo round-trips cleanly.
const envKeySchema = z
  .string()
  .regex(/^[A-Z_][A-Z0-9_]*$/, "key must be UPPER_SNAKE_CASE (letters, digits, underscores)");

const envMapSchema = z.record(envKeySchema, z.string()).optional().default({});

const writeSchema = z.object({
  name: z
    .string()
    .min(1, "name is required")
    .regex(/^[A-Za-z0-9._-]+$/, "name: letters, digits, dash, underscore, dot only"),
  description: z.string().optional().default(""),
  engine: z.literal("kubernetes"),
  default_image: z.string().optional().default(""),
  default_cpu_request: quantitySchema.optional().default(""),
  default_cpu_limit: quantitySchema.optional().default(""),
  default_mem_request: quantitySchema.optional().default(""),
  default_mem_limit: quantitySchema.optional().default(""),
  max_cpu: quantitySchema.optional().default(""),
  max_mem: quantitySchema.optional().default(""),
  tags: z.array(tagSchema).optional().default([]),
  // Plain env vars the runner injects into every plugin container
  // running on this profile. Use for non-secret config like bucket
  // names and regions.
  env: envMapSchema,
  // Encrypted at rest. The server seals each value with the AEAD
  // cipher (GOCDNEXT_SECRET_KEY) before persisting; reads return
  // only the keys, never the values. Empty/missing on update means
  // "remove every secret on this profile" — full-replace semantics.
  secrets: envMapSchema,
});

const updateSchema = writeSchema.extend({ id: z.string().min(1) });
const deleteSchema = z.object({ id: z.string().min(1) });

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

export async function createRunnerProfile(
  input: z.infer<typeof writeSchema>,
): Promise<ActionResult> {
  const parsed = writeSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch("/api/v1/admin/runner-profiles", {
      method: "POST",
      body: JSON.stringify(parsed.data),
    });
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/profiles");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function updateRunnerProfile(
  input: z.infer<typeof updateSchema>,
): Promise<ActionResult> {
  const parsed = updateSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const { id, ...body } = parsed.data;
    const res = await apiFetch(
      `/api/v1/admin/runner-profiles/${encodeURIComponent(id)}`,
      { method: "PUT", body: JSON.stringify(body) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/profiles");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function deleteRunnerProfile(
  input: z.infer<typeof deleteSchema>,
): Promise<ActionResult> {
  const parsed = deleteSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch(
      `/api/v1/admin/runner-profiles/${encodeURIComponent(parsed.data.id)}`,
      { method: "DELETE" },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/admin/profiles");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
