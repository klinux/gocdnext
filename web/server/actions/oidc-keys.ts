"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

// Mode is a closed enum on purpose: the server treats an EMPTY mode
// as graceful, so a typo'd value reaching the wire could silently
// downgrade an intended emergency rotation. Rejecting client-side
// keeps the failure loud and local.
const rotateSchema = z.object({
  mode: z.enum(["graceful", "emergency"]),
});

export type RotateResult =
  | { ok: true; data: { kid: string; mode: string; note: string } }
  | { ok: false; error: string };

export async function rotateOIDCKey(
  input: z.infer<typeof rotateSchema>,
): Promise<RotateResult> {
  const parsed = rotateSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      "/api/v1/admin/oidc/keys/rotate";
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ mode: parsed.data.mode }),
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 200) || "rotation failed"}`,
      };
    }
    const data = (await res.json()) as {
      kid: string;
      mode: string;
      note: string;
    };
    revalidatePath("/settings/oidc");
    return { ok: true, data };
  } catch (err) {
    return { ok: false, error: errorMessage(err) };
  }
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
