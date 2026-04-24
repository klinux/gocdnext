"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";
import { env } from "@/lib/env";

const notificationSchema = z.object({
  on: z.enum(["failure", "success", "always", "canceled"]),
  uses: z.string().min(1, "uses is required"),
  with: z.record(z.string(), z.string()).optional(),
  secrets: z.array(z.string()).optional(),
});

const setSchema = z.object({
  slug: z.string().min(1),
  notifications: z.array(notificationSchema),
});

export type ActionResult = { ok: true } | { ok: false; error: string };

export async function setProjectNotifications(
  input: z.infer<typeof setSchema>,
): Promise<ActionResult> {
  const parsed = setSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/notifications`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "PUT",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ notifications: parsed.data.notifications }),
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 300) || "save failed"}`,
      };
    }
    revalidatePath(`/projects/${parsed.data.slug}/notifications`);
    return { ok: true };
  } catch (err) {
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}
