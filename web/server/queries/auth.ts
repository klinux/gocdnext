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
// mode where auth is disabled server-side).
export type AuthState =
  | { mode: "disabled" }
  | { mode: "anonymous"; providers: AuthProvidersResponse["providers"] }
  | { mode: "authenticated"; user: CurrentUser };

export async function resolveAuthState(): Promise<AuthState> {
  const [providersRes, meRes] = await Promise.all([
    forwardCookieFetch("/auth/providers"),
    forwardCookieFetch("/api/v1/me"),
  ]);

  const providers: AuthProvidersResponse = providersRes.ok
    ? ((await providersRes.json()) as AuthProvidersResponse)
    : { enabled: false, providers: [] };

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
