// RSC-only helpers for per-user preferences. Forwards the session
// cookie to /api/v1/account/preferences the same way other RSC
// fetches do — the control plane tests RequireAuth at the mount
// point and we need to present the user's session.

import { cookies } from "next/headers";

import { env } from "@/lib/env";
import type {
  UserPreferences,
  UserPreferencesResponse,
} from "@/types/api";

const SESSION_COOKIE = "gocdnext_session";

export async function getUserPreferences(): Promise<UserPreferences> {
  const url =
    env.GOCDNEXT_API_URL.replace(/\/+$/, "") + "/api/v1/account/preferences";
  const store = await cookies();
  const session = store.get(SESSION_COOKIE)?.value;
  const res = await fetch(url, {
    cache: "no-store",
    headers: {
      Accept: "application/json",
      ...(session ? { Cookie: `${SESSION_COOKIE}=${session}` } : {}),
    },
  });
  // Auth off (dev) / not logged in / cold DB — any of those mean
  // "render with empty preferences"; the page still works, the
  // user just sees the default view. Only log the surprise cases.
  if (!res.ok) {
    return {};
  }
  const body = (await res.json()) as UserPreferencesResponse;
  return body.preferences ?? {};
}
