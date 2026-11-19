import { afterEach, describe, expect, it, vi } from "vitest";

import { type AuthState, isAdmin, isMaintainer, resolveAuthState } from "./auth";

vi.mock("next/headers", () => ({
  cookies: async () => ({ get: () => undefined }),
}));

// Route fetch by path so each scenario can set /auth/providers and /api/v1/me
// independently. `res(status, body)` builds a minimal Response-like stub.
function res(ok: boolean, body: unknown) {
  return { ok, status: ok ? 200 : 401, json: async () => body } as Response;
}

function stubFetch(routes: {
  providers: () => Response;
  me: () => Response;
}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) =>
      Promise.resolve(
        url.includes("/api/v1/me") ? routes.me() : routes.providers(),
      ),
    ),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("resolveAuthState", () => {
  it("returns disabled only on a clean providers response with enabled:false", async () => {
    stubFetch({
      providers: () => res(true, { enabled: false, providers: [] }),
      me: () => res(false, {}),
    });
    expect(await resolveAuthState()).toEqual({ mode: "disabled" });
  });

  it("fails closed to unknown (NOT disabled) when /auth/providers errors and no user resolves", async () => {
    // The bug this guards: a providers 500 must not masquerade as "disabled",
    // which would grant maintainer actions to a viewer.
    stubFetch({
      providers: () => res(false, {}),
      me: () => res(false, {}),
    });
    expect(await resolveAuthState()).toEqual({ mode: "unknown" });
  });

  it("still trusts a resolved user even if the providers probe flaked", async () => {
    const user = { id: "u1", role: "maintainer" };
    stubFetch({
      providers: () => res(false, {}),
      me: () => res(true, { user }),
    });
    expect(await resolveAuthState()).toMatchObject({
      mode: "authenticated",
      user: { role: "maintainer" },
    });
  });

  it("is authenticated when auth is on and /me resolves", async () => {
    stubFetch({
      providers: () => res(true, { enabled: true, providers: [] }),
      me: () => res(true, { user: { id: "u1", role: "admin" } }),
    });
    expect(await resolveAuthState()).toMatchObject({ mode: "authenticated" });
  });

  it("is anonymous when auth is on but no user resolves", async () => {
    stubFetch({
      providers: () => res(true, { enabled: true, providers: [{ id: "oidc" }] }),
      me: () => res(false, {}),
    });
    expect(await resolveAuthState()).toMatchObject({ mode: "anonymous" });
  });
});

// Transport failures must degrade, not crash: a rejected fetch, a body that
// isn't JSON, or a /me payload without a usable user all fail CLOSED to
// "unknown" (layout renders, privileged nav hidden) instead of a 500.
describe("resolveAuthState transport failures", () => {
  const badJSON = () =>
    ({
      ok: true,
      json: async () => {
        throw new SyntaxError("Unexpected token <");
      },
    }) as unknown as Response;

  function stubRaw(providers: () => Promise<Response>, me: () => Promise<Response>) {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => (url.includes("/api/v1/me") ? me() : providers())),
    );
  }

  const enabled = () => Promise.resolve(res(true, { enabled: true, providers: [] }));
  const reject = () => Promise.reject(new TypeError("fetch failed"));

  const tests: {
    name: string;
    providers: () => Promise<Response>;
    me: () => Promise<Response>;
  }[] = [
    { name: "both fetches reject", providers: reject, me: reject },
    { name: "/me rejects", providers: enabled, me: reject },
    { name: "providers rejects, /me 401", providers: reject, me: () => Promise.resolve(res(false, {})) },
    { name: "providers body is not JSON", providers: () => Promise.resolve(badJSON()), me: () => Promise.resolve(res(false, {})) },
    { name: "/me body is not JSON", providers: enabled, me: () => Promise.resolve(badJSON()) },
    { name: "/me payload has no user", providers: enabled, me: () => Promise.resolve(res(true, {})) },
    {
      name: "/me user has an unknown role",
      providers: enabled,
      me: () => Promise.resolve(res(true, { user: { id: "u1", role: "superuser" } })),
    },
  ];
  for (const tt of tests) {
    it(`${tt.name} → unknown`, async () => {
      stubRaw(tt.providers, tt.me);
      expect(await resolveAuthState()).toEqual({ mode: "unknown" });
    });
  }

  it("still trusts a valid user when only the providers fetch rejects", async () => {
    stubRaw(reject, () => Promise.resolve(res(true, { user: { id: "u1", role: "viewer" } })));
    expect(await resolveAuthState()).toMatchObject({
      mode: "authenticated",
      user: { role: "viewer" },
    });
  });
});

// Only a 401 from /me means "logged out" (→ /login). Any other non-OK status
// is a control-plane problem: redirecting to /login there would loop, since
// login lands back on a page whose /me still fails.
describe("resolveAuthState /me status", () => {
  const status = (code: number) =>
    ({ ok: code >= 200 && code < 300, status: code, json: async () => ({}) }) as Response;
  const providersOn = () => res(true, { enabled: true, providers: [] });

  it("401 → anonymous", async () => {
    stubFetch({ providers: providersOn, me: () => status(401) });
    expect(await resolveAuthState()).toMatchObject({ mode: "anonymous" });
  });

  it.each([403, 404, 500, 502, 503])("%i → unknown", async (code) => {
    stubFetch({ providers: providersOn, me: () => status(code) });
    expect(await resolveAuthState()).toEqual({ mode: "unknown" });
  });
});

// A /me user with only id+role still renders: display fields are normalized
// to strings so the account menu's `name || email` can't blow up the layout.
describe("resolveAuthState user normalization", () => {
  it("fills missing display fields with empty strings", async () => {
    stubFetch({
      providers: () => res(true, { enabled: true, providers: [] }),
      me: () => res(true, { user: { id: "u1", role: "viewer", name: 42 } }),
    });
    const auth = await resolveAuthState();
    expect(auth).toMatchObject({
      mode: "authenticated",
      user: { id: "u1", role: "viewer", name: "", email: "", provider: "" },
    });
  });

  it("drops a non-string avatar_url", async () => {
    stubFetch({
      providers: () => res(true, { enabled: true, providers: [] }),
      me: () =>
        res(true, { user: { id: "u1", role: "admin", avatar_url: { x: 1 } } }),
    });
    const auth = await resolveAuthState();
    expect(auth.mode === "authenticated" && auth.user.avatar_url).toBe(undefined);
  });
});

// Nav gating mirrors the server's RequireMinRole: admin ≥ maintainer ≥ viewer,
// everything passes when auth is genuinely disabled, and "unknown"/"anonymous"
// fail CLOSED so a flaky providers probe can't surface admin menus.
describe("role gates", () => {
  const as = (role: "admin" | "maintainer" | "viewer"): AuthState =>
    ({ mode: "authenticated", user: { role } }) as AuthState;

  const tests: { name: string; auth: AuthState; admin: boolean; maintainer: boolean }[] = [
    { name: "admin", auth: as("admin"), admin: true, maintainer: true },
    { name: "maintainer", auth: as("maintainer"), admin: false, maintainer: true },
    { name: "viewer", auth: as("viewer"), admin: false, maintainer: false },
    { name: "auth disabled", auth: { mode: "disabled" }, admin: true, maintainer: true },
    { name: "unknown fails closed", auth: { mode: "unknown" }, admin: false, maintainer: false },
    {
      name: "anonymous",
      auth: { mode: "anonymous", providers: [] },
      admin: false,
      maintainer: false,
    },
  ];
  for (const tt of tests) {
    it(tt.name, () => {
      expect(isAdmin(tt.auth)).toBe(tt.admin);
      expect(isMaintainer(tt.auth)).toBe(tt.maintainer);
    });
  }
});
