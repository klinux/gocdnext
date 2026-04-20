"use server";

import { cookies } from "next/headers";
import { z } from "zod";

import { env } from "@/lib/env";

// loginLocal POSTs the control plane's /auth/login/local and
// carries the Set-Cookie header back to the browser via the
// Next.js cookies() store. Returns either a redirect target on
// success or a user-facing error string on failure.

const loginSchema = z.object({
  email: z.string().email(),
  password: z.string().min(1).max(512),
  next: z.string().optional(),
});

export type LocalLoginResult =
  | { ok: true; next: string }
  | { ok: false; error: string };

export async function loginLocal(
  input: z.infer<typeof loginSchema>,
): Promise<LocalLoginResult> {
  const parsed = loginSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + "/auth/login/local";
  try {
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify({ email: parsed.data.email, password: parsed.data.password }),
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: friendlyError(res.status, body),
      };
    }
    // Extract the session cookie the control plane set and attach
    // it to the browser response via next/headers.
    const setCookie = res.headers.get("Set-Cookie") ?? "";
    const token = extractSessionToken(setCookie);
    if (token) {
      const store = await cookies();
      store.set({
        name: "gocdnext_session",
        value: token,
        path: "/",
        httpOnly: true,
        sameSite: "lax",
        // Secure tracks whether the control plane is on HTTPS.
        // For dev (http), the control plane sent Secure=false
        // and we honor that here.
        secure: setCookie.toLowerCase().includes("secure"),
      });
    }
    return { ok: true, next: sanitizeNext(parsed.data.next ?? "/") };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

function extractSessionToken(header: string): string | null {
  // Naive cookie parser — Set-Cookie has only one cookie per
  // header here (control plane sets just gocdnext_session on
  // login). Split on `;` and find the k=v pair.
  const parts = header.split(",").flatMap((c) => c.split(";"));
  for (const p of parts) {
    const trimmed = p.trim();
    if (trimmed.startsWith("gocdnext_session=")) {
      return trimmed.slice("gocdnext_session=".length);
    }
  }
  return null;
}

function sanitizeNext(v: string): string {
  if (!v.startsWith("/") || v.startsWith("//")) return "/";
  return v;
}

function friendlyError(status: number, body: string): string {
  if (status === 401) return "Invalid email or password.";
  if (status === 403) return "Account disabled.";
  if (status === 429) return "Too many attempts. Try again in a few minutes.";
  const snippet = body.trim().slice(0, 200);
  return snippet || `server error ${status}`;
}
