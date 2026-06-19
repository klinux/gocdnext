"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";
import { env } from "@/lib/env";
import { secretNameSchema, secretPayloadSchema } from "@/lib/validations";

// Project-scoped set carries the slug alongside the secret payload.
// `slug` is split out before the discriminated-union parse so the
// union stays scope-agnostic (the global action reuses it verbatim).
const setSchema = z.object({
  slug: z.string().min(1),
  payload: secretPayloadSchema,
});

const deleteSchema = z.object({
  slug: z.string().min(1),
  name: secretNameSchema,
});

export type ActionResult =
  | { ok: true; created?: boolean }
  | { ok: false; error: string };

// requestBody turns a parsed payload into the wire shape the server
// accepts: { name, value?, source?, ref? }. The DB variant sends the
// inline value and lets source default server-side; external variants
// send source + ref and never a value.
function requestBody(payload: z.infer<typeof secretPayloadSchema>): Record<string, unknown> {
  if (payload.source === "db") {
    return { name: payload.name, value: payload.value };
  }
  return { name: payload.name, source: payload.source, ref: payload.ref };
}

export async function setSecret(input: z.infer<typeof setSchema>): Promise<ActionResult> {
  const parsed = setSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await postJSON(
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/secrets`,
      requestBody(parsed.data.payload),
    );
    revalidatePath(`/projects/${parsed.data.slug}/secrets`);
    return { ok: true, created: res.created ?? false };
  } catch (err) {
    return { ok: false, error: errorMessage(err) };
  }
}

export async function deleteSecret(input: z.infer<typeof deleteSchema>): Promise<ActionResult> {
  const parsed = deleteSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/secrets/${encodeURIComponent(parsed.data.name)}`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "DELETE",
      cache: "no-store",
      headers: session ? { Cookie: `gocdnext_session=${session}` } : {},
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 200) || "delete failed"}`,
      };
    }
    revalidatePath(`/projects/${parsed.data.slug}/secrets`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: errorMessage(err) };
  }
}

const globalDeleteSchema = z.object({
  name: secretNameSchema,
});

export async function setGlobalSecret(
  input: z.infer<typeof secretPayloadSchema>,
): Promise<ActionResult> {
  const parsed = secretPayloadSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await postJSON(`/api/v1/admin/secrets`, requestBody(parsed.data));
    revalidatePath(`/admin/secrets`);
    return { ok: true, created: res.created ?? false };
  } catch (err) {
    return { ok: false, error: errorMessage(err) };
  }
}

export async function deleteGlobalSecret(
  input: z.infer<typeof globalDeleteSchema>,
): Promise<ActionResult> {
  const parsed = globalDeleteSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/admin/secrets/${encodeURIComponent(parsed.data.name)}`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "DELETE",
      cache: "no-store",
      headers: session ? { Cookie: `gocdnext_session=${session}` } : {},
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 200) || "delete failed"}`,
      };
    }
    revalidatePath(`/admin/secrets`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: errorMessage(err) };
  }
}

async function postJSON(path: string, body: unknown): Promise<{ created?: boolean }> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  // Forward session cookie so the backend's RequireAuth middleware
  // accepts the request. Server actions don't inherit browser
  // cookies automatically — omitting this returned "not authenticated".
  const session = (await cookies()).get("gocdnext_session")?.value;
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
    throw new Error(`server ${res.status}: ${text.trim().slice(0, 200) || "post failed"}`);
  }
  return text ? (JSON.parse(text) as { created?: boolean }) : {};
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
