"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

const purgeSchema = z.object({
  slug: z.string().min(1),
  // cacheID is a UUID on the server side but we don't gain
  // anything by enforcing the shape client-side — let the API
  // return 400 on a malformed id so the handler stays the one
  // source of truth for id validation.
  cacheID: z.string().min(1),
});

export type ActionResult = { ok: true } | { ok: false; error: string };

export async function purgeCache(
  input: z.infer<typeof purgeSchema>,
): Promise<ActionResult> {
  const parsed = purgeSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/caches/${encodeURIComponent(parsed.data.cacheID)}`;
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
        error: `server ${res.status}: ${body.trim().slice(0, 200) || "purge failed"}`,
      };
    }
    revalidatePath(`/projects/${parsed.data.slug}/caches`);
    return { ok: true };
  } catch (err) {
    return { ok: false, error: errorMessage(err) };
  }
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
