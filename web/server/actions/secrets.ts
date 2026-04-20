"use server";

import { revalidatePath } from "next/cache";
import { z } from "zod";
import { env } from "@/lib/env";
import { secretNameSchema } from "@/lib/validations";

const setSchema = z.object({
  slug: z.string().min(1),
  name: secretNameSchema,
  value: z.string().min(1, "value cannot be empty").max(64 * 1024),
});

const deleteSchema = z.object({
  slug: z.string().min(1),
  name: secretNameSchema,
});

export type ActionResult =
  | { ok: true; created?: boolean }
  | { ok: false; error: string };

export async function setSecret(input: z.infer<typeof setSchema>): Promise<ActionResult> {
  const parsed = setSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const res = await postJSON(`/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/secrets`, {
      name: parsed.data.name,
      value: parsed.data.value,
    });
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
    const res = await fetch(url, { method: "DELETE", cache: "no-store" });
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

async function postJSON(path: string, body: unknown): Promise<{ created?: boolean }> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  const res = await fetch(url, {
    method: "POST",
    cache: "no-store",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
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
