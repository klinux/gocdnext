"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";
import {
  secretBackendSources,
  secretBackendWriteSchema,
} from "@/lib/validations";
import type { SecretBackend, SecretBackendProbeResult } from "@/types/api";

// The set/test actions share the write body shape. delete only needs
// the source (the URL segment).
const sourceSchema = z.object({ source: z.enum(secretBackendSources) });

export type ActionResult<T = void> =
  | { ok: true; data: T }
  | { ok: false; error: string };

// TestResult mirrors the cluster test shape: a probe envelope on
// success, an error string on transport/validation failure. The probe
// itself always carries a status (even "error"), so a red chip on the
// UI distinguishes a failed probe from a failed request.
export type TestResult =
  | { ok: true; probe: SecretBackendProbeResult }
  | { ok: false; error: string };

const BASE = "/api/v1/admin/secret-backends";

async function apiFetch(path: string, init: RequestInit): Promise<Response> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  const session = (await cookies()).get("gocdnext_session")?.value;
  return fetch(url, {
    cache: "no-store",
    ...init,
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
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

// writeBody turns the validated form input into the wire shape the PUT
// endpoint accepts. `credentials` is omitted entirely when absent
// (gcp/aws, or a preserve-on-edit) so the server never sees an empty
// object it has to special-case.
function writeBody(
  parsed: z.infer<typeof secretBackendWriteSchema>,
): Record<string, unknown> {
  const body: Record<string, unknown> = {
    enabled: parsed.enabled,
    value: parsed.value,
  };
  if (parsed.credentials && Object.keys(parsed.credentials).length > 0) {
    body.credentials = parsed.credentials;
  }
  if (parsed.preserve_credentials) {
    body.preserve_credentials = true;
  }
  return body;
}

export async function setSecretBackend(
  input: z.infer<typeof secretBackendWriteSchema>,
): Promise<ActionResult<SecretBackend>> {
  const parsed = secretBackendWriteSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const res = await apiFetch(
      `${BASE}/${encodeURIComponent(parsed.data.source)}`,
      { method: "PUT", body: JSON.stringify(writeBody(parsed.data)) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    const data = (await res.json()) as SecretBackend;
    revalidatePath("/settings/secret-backends");
    return { ok: true, data };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

export async function deleteSecretBackend(
  input: z.infer<typeof sourceSchema>,
): Promise<ActionResult> {
  const parsed = sourceSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const res = await apiFetch(
      `${BASE}/${encodeURIComponent(parsed.data.source)}`,
      { method: "DELETE" },
    );
    if (!res.ok) return errorResult(res, await res.text());
    revalidatePath("/settings/secret-backends");
    return { ok: true, data: undefined };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

// testSecretBackend runs the server-side connectivity probe with the
// submitted (or, when preserve_credentials, the stored) credential. Not
// a mutation — NO revalidation. The endpoint always returns HTTP 200
// with a status field, so a non-ok probe still arrives via { ok: true }.
export async function testSecretBackend(
  input: z.infer<typeof secretBackendWriteSchema>,
): Promise<TestResult> {
  const parsed = secretBackendWriteSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await apiFetch(
      `${BASE}/${encodeURIComponent(parsed.data.source)}/test`,
      { method: "POST", body: JSON.stringify(writeBody(parsed.data)) },
    );
    if (!res.ok) return errorResult(res, await res.text());
    const probe = (await res.json()) as SecretBackendProbeResult;
    return { ok: true, probe };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
