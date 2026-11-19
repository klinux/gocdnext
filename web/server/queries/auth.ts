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

// isAdmin / isMaintainer mirror the server's RequireMinRole hierarchy
// (admin ≥ maintainer ≥ viewer) for UI gating — hiding nav that would only
// lead into a 403. "disabled" passes everything, exactly like the server
// middleware; "unknown"/"anonymous" fail closed. The server stays the
// enforcement point; these only decide what to show.
export function isAdmin(auth: AuthState): boolean {
  return (
    auth.mode === "disabled" ||
    (auth.mode === "authenticated" && auth.user.role === "admin")
  );
}

export function isMaintainer(auth: AuthState): boolean {
  return (
    auth.mode === "disabled" ||
    (auth.mode === "authenticated" &&
      (auth.user.role === "admin" || auth.user.role === "maintainer"))
  );
}

// Probe is one auth endpoint's outcome. "http-error" = the server answered
// non-OK, with its status (a 401 from /me is the normal logged-out case);
// "failed" = we never got a usable answer (network error, timeout, body that
// isn't JSON).
type Probe =
  | { kind: "ok"; body: unknown }
  | { kind: "http-error"; status: number }
  | { kind: "failed" };

async function probe(path: string): Promise<Probe> {
  try {
    const res = await forwardCookieFetch(path);
    if (!res.ok) return { kind: "http-error", status: res.status };
    return { kind: "ok", body: (await res.json()) as unknown };
  } catch {
    // Swallowed on purpose: the caller maps "failed" to the fail-closed
    // "unknown" state, so an auth-probe outage renders the dashboard with
    // privileged nav hidden instead of 500ing every page. The server's
    // RequireMinRole still enforces every privileged read.
    return { kind: "failed" };
  }
}

const ROLES: ReadonlySet<string> = new Set(["admin", "maintainer", "viewer"]);

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null;
}

const str = (v: unknown): string => (typeof v === "string" ? v : "");

// parseUser accepts only a /me payload whose user carries an id and a role we
// know — the fields every gate reads; anything else is "no usable user", never
// a guess. Display fields are normalized to strings rather than rejected, so a
// partial payload can't crash the account menu (`name || email`) while a real
// user missing, say, an email still gets in.
function parseUser(body: unknown): CurrentUser | null {
  if (!isRecord(body) || !isRecord(body.user)) return null;
  const u = body.user;
  if (typeof u.id !== "string" || u.id === "") return null;
  if (typeof u.role !== "string" || !ROLES.has(u.role)) return null;
  const user: CurrentUser = {
    id: u.id,
    // Narrowed by the ROLES check above; TS can't follow a Set membership.
    role: u.role as CurrentUser["role"],
    email: str(u.email),
    name: str(u.name),
    provider: str(u.provider),
    external_id: str(u.external_id),
    created_at: str(u.created_at),
    updated_at: str(u.updated_at),
  };
  if (typeof u.avatar_url === "string") user.avatar_url = u.avatar_url;
  if (typeof u.last_login_at === "string") user.last_login_at = u.last_login_at;
  return user;
}

function parseProviders(body: unknown): AuthProvidersResponse | null {
  if (!isRecord(body) || typeof body.enabled !== "boolean") return null;
  return {
    enabled: body.enabled,
    providers: Array.isArray(body.providers)
      ? (body.providers as AuthProvidersResponse["providers"])
      : [],
  };
}

export async function resolveAuthState(): Promise<AuthState> {
  const [providersProbe, meProbe] = await Promise.all([
    probe("/auth/providers"),
    probe("/api/v1/me"),
  ]);

  const user = meProbe.kind === "ok" ? parseUser(meProbe.body) : null;
  // Only a 401 means "logged out". /me that failed, answered garbage, or
  // answered any other non-OK (403/5xx…) means we don't know who this is: we
  // must not bounce them to /login (it would loop while /me stays broken)
  // nor grant anything.
  const loggedOut = meProbe.kind === "http-error" && meProbe.status === 401;

  const providers =
    providersProbe.kind === "ok" ? parseProviders(providersProbe.body) : null;

  // An unusable /auth/providers must NOT masquerade as "auth disabled": that
  // would fail OPEN on privilege gates. A resolved user is still
  // authoritative (auth is on, role known); otherwise fail closed.
  if (providers === null) {
    return user ? { mode: "authenticated", user } : { mode: "unknown" };
  }
  if (!providers.enabled) {
    return { mode: "disabled" };
  }
  if (user) {
    return { mode: "authenticated", user };
  }
  if (!loggedOut) {
    return { mode: "unknown" };
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
