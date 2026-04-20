"use server";

import { cookies } from "next/headers";

import { env } from "@/lib/env";

// logoutAction POSTs the control plane's /auth/logout, then deletes
// the cookie on our own origin too so the next RSC render sees a
// fresh anonymous state. The Next.js dev server proxies its own
// Set-Cookie back to the browser, so we also drop our copy here.
export async function logoutAction(): Promise<{ ok: true } | { ok: false; error: string }> {
  const store = await cookies();
  const session = store.get("gocdnext_session")?.value;
  try {
    await fetch(env.GOCDNEXT_API_URL.replace(/\/+$/, "") + "/auth/logout", {
      method: "POST",
      cache: "no-store",
      headers: session ? { Cookie: `gocdnext_session=${session}` } : {},
    });
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
  store.delete("gocdnext_session");
  return { ok: true };
}
