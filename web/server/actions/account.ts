"use server";

import { cookies } from "next/headers";
import { z } from "zod";

import { env } from "@/lib/env";

const schema = z.object({
  current_password: z.string().min(1),
  new_password: z.string().min(8).max(512),
});

export type AccountActionResult =
  | { ok: true }
  | { ok: false; error: string };

export async function changeOwnPassword(
  input: z.infer<typeof schema>,
): Promise<AccountActionResult> {
  const parsed = schema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + "/api/v1/me/password";
  const store = await cookies();
  const session = store.get("gocdnext_session")?.value;
  try {
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify(parsed.data),
    });
    if (!res.ok) {
      const body = await res.text();
      const msg =
        res.status === 401
          ? "Current password is incorrect."
          : res.status === 403
            ? "Password change is only available for local accounts."
            : res.status === 429
              ? "Too many attempts. Try again in a few minutes."
              : body.trim().slice(0, 200) || `server ${res.status}`;
      return { ok: false, error: msg };
    }
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
