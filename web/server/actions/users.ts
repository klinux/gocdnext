"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

const setRoleSchema = z.object({
  userID: z.string().min(1),
  role: z.enum(["admin", "maintainer", "viewer"]),
});

export type ActionResult = { ok: true } | { ok: false; error: string };

// setUserRole POSTs the new role to the admin endpoint. The
// server-side store refuses self-demotion (403) and malformed
// role (400) — we surface those error messages to the UI toast
// so operators understand why the action didn't stick.
export async function setUserRole(
  input: z.infer<typeof setRoleSchema>,
): Promise<ActionResult> {
  const parsed = setRoleSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/admin/users/${encodeURIComponent(parsed.data.userID)}/role`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "PUT",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ role: parsed.data.role }),
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 200) || "role change failed"}`,
      };
    }
    revalidatePath("/admin/users");
    revalidatePath("/admin/audit");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

const createUserSchema = z.object({
  email: z.string().email(),
  name: z.string().max(160).optional().default(""),
  role: z.enum(["admin", "maintainer", "viewer"]),
  // The backend's local-account password policy enforces the
  // real lower bound; we only block the obviously-wrong here so
  // the dialog can react before a round-trip.
  password: z.string().min(8).max(512),
});

// createLocalUser provisions a password-backed account through
// the admin endpoint. 409 (duplicate email) and 400 (weak
// password / bad role) come back as ok=false with the server's
// message; the dialog renders them inline.
export async function createLocalUser(
  input: z.infer<typeof createUserSchema>,
): Promise<ActionResult> {
  const parsed = createUserSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") + "/api/v1/admin/users";
    const session = (await cookies()).get("gocdnext_session")?.value;
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
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 200) || "create failed"}`,
      };
    }
    revalidatePath("/admin/users");
    revalidatePath("/admin/audit");
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
