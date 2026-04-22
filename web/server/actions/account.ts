"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";
import type { UserPreferences } from "@/types/api";

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

const preferencesSchema = z.object({
  hidden_projects: z.array(z.string().uuid()).optional(),
});

export type SavePreferencesResult =
  | { ok: true; preferences: UserPreferences }
  | { ok: false; error: string };

// savePreferences PUTs the full preferences document. The UI owns
// the merge (load, edit, save) — the server replaces wholesale,
// which keeps this action small and the wire shape predictable.
// revalidatePath("/projects") so RSC re-renders with the new
// hidden-project filter without a manual reload.
export async function savePreferences(
  prefs: UserPreferences,
): Promise<SavePreferencesResult> {
  const parsed = preferencesSchema.safeParse(prefs);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid preferences",
    };
  }
  const url =
    env.GOCDNEXT_API_URL.replace(/\/+$/, "") + "/api/v1/account/preferences";
  const store = await cookies();
  const session = store.get("gocdnext_session")?.value;
  try {
    const res = await fetch(url, {
      method: "PUT",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ preferences: parsed.data }),
    });
    const text = await res.text();
    if (!res.ok) {
      return {
        ok: false,
        error: text.trim().slice(0, 200) || `server ${res.status}`,
      };
    }
    const body = text
      ? (JSON.parse(text) as { preferences?: UserPreferences })
      : {};
    revalidatePath("/projects");
    return { ok: true, preferences: body.preferences ?? parsed.data };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
