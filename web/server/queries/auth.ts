// RSC-only auth helpers. Forward the browser's session cookie to the
// control plane so /api/v1/me can resolve the current user — Next's
// fetch doesn't carry cookies automatically across a server-side call.

import { cookies } from "next/headers";

import { env } from "@/lib/env";
import type { AuthProvidersResponse, CurrentUser } from "@/types/api";

const SESSION_COOKIE = "gocdnext_session";

async function forwardCookieFetch(path: string): Promise<Response> {
  const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
  const store = await cookies();
  const session = store.get(SESSION_COOKIE)?.value;
  return fetch(url, {
    cache: "no-store",
    headers: {
      Accept: "application/json",
      ...(session ? { Cookie: `${SESSION_COOKIE}=${session}` } : {}),
    },
  });
}

// AuthState is the combined truth the dashboard layout uses to
// decide: render the app, redirect to /login, or stay anonymous (dev
// mode where auth is disabled server-side). "unknown" = the auth config
// couldn't be determined (the /auth/providers probe errored and no user
// resolved) — the layout still renders (it only redirects on "anonymous"),
// but privilege gates MUST fail closed on it rather than treat it as
// "disabled" (which grants everything).
export type AuthState =
  | { mode: "disabled" }
  | { mode: "unknown" }
  | { mode: "anonymous"; providers: AuthProvidersResponse["providers"] }
  | { mode: "authenticated"; user: CurrentUser };

export async function resolveAuthState(): Promise<AuthState> {
  const [providersRes, meRes] = await Promise.all([
    forwardCookieFetch("/auth/providers"),
    forwardCookieFetch("/api/v1/me"),
  ]);

  // A non-OK /auth/providers must NOT masquerade as "auth disabled": that
  // would fail OPEN on privilege gates (a viewer briefly seeing maintainer
  // actions during a transient 500). Only a clean response lets us tell
  // "genuinely disabled" from "config unreachable".
  if (!providersRes.ok) {
    // A resolved user is still authoritative (auth is on, role known) even if
    // the providers probe flaked; otherwise we can't distinguish disabled from
    // logged-out, so fail closed to "unknown".
    if (meRes.ok) {
      const payload = (await meRes.json()) as { user: CurrentUser };
      return { mode: "authenticated", user: payload.user };
    }
    return { mode: "unknown" };
  }

  const providers = (await providersRes.json()) as AuthProvidersResponse;
  if (!providers.enabled) {
    return { mode: "disabled" };
  }
  if (meRes.ok) {
    const payload = (await meRes.json()) as { user: CurrentUser };
    return { mode: "authenticated", user: payload.user };
  }
  return { mode: "anonymous", providers: providers.providers };
}

export async function listProviders(): Promise<AuthProvidersResponse> {
  const res = await forwardCookieFetch("/auth/providers");
  if (!res.ok) {
    return { enabled: false, providers: [] };
  }
  return (await res.json()) as AuthProvidersResponse;
}
