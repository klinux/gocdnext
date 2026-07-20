import { afterEach, describe, expect, it, vi } from "vitest";

import { resolveAuthState } from "./auth";

vi.mock("next/headers", () => ({
  cookies: async () => ({ get: () => undefined }),
}));

// Route fetch by path so each scenario can set /auth/providers and /api/v1/me
// independently. `res(status, body)` builds a minimal Response-like stub.
function res(ok: boolean, body: unknown) {
  return { ok, json: async () => body } as Response;
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
