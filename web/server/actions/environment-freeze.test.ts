import { beforeEach, describe, expect, it, vi } from "vitest";

// "use server" module: stub the Next-only bits so the test can assert the exact
// request that reaches the API. A schema that parses is not proof the URL, verb
// and body survive the round-trip.
vi.mock("next/headers", () => ({
  cookies: async () => ({ get: () => ({ value: "session-token" }) }),
}));
vi.mock("next/cache", () => ({ revalidatePath: vi.fn() }));
vi.mock("@/lib/env", () => ({ env: { GOCDNEXT_API_URL: "http://api.test" } }));

import { freezeEnvironment, unfreezeEnvironment } from "./environments";

const fetchMock = vi.fn(async () => new Response("{}", { status: 200 }));
beforeEach(() => {
  fetchMock.mockClear();
  vi.stubGlobal("fetch", fetchMock);
});

function call(): [string, RequestInit] {
  return fetchMock.mock.calls[0] as unknown as [string, RequestInit];
}

describe("freezeEnvironment", () => {
  it("PUTs to the name-keyed endpoint with the reason", async () => {
    const res = await freezeEnvironment({
      slug: "acme",
      name: "production",
      reason: "month-end close",
    });
    expect(res.ok).toBe(true);
    const [url, init] = call();
    expect(url).toBe(
      "http://api.test/api/v1/projects/acme/environment-freezes/production",
    );
    expect(init.method).toBe("PUT");
    expect(JSON.parse(String(init.body))).toEqual({ reason: "month-end close" });
  });

  it("rejects an empty reason without calling the API", async () => {
    // A freeze with no stated reason is the failure this feature exists to
    // prevent — it must not even reach the server.
    const res = await freezeEnvironment({
      slug: "acme",
      name: "production",
      reason: "   ",
    });
    expect(res.ok).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("rejects a reason over the 500-character bound before the round-trip", async () => {
    const res = await freezeEnvironment({
      slug: "acme",
      name: "production",
      reason: "x".repeat(501),
    });
    expect(res.ok).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("rejects a name outside the environment grammar", async () => {
    // A '/' would also break out of the endpoint's path segment, so this is a
    // correctness guard as much as a validation one.
    for (const name of ["prod/uction", "-prod", "", "a".repeat(65)]) {
      const res = await freezeEnvironment({ slug: "acme", name, reason: "x" });
      expect(res.ok).toBe(false);
    }
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("percent-encodes the environment name in the URL", async () => {
    await freezeEnvironment({
      slug: "acme",
      name: "staging.eu-west_1",
      reason: "audit",
    });
    expect(call()[0]).toContain("/environment-freezes/staging.eu-west_1");
  });

  it("surfaces the server's message verbatim on a refusal", async () => {
    // The handler writes user-ready bodies per status (not a maintainer,
    // invalid name); paraphrasing them here would lose the actionable part.
    fetchMock.mockResolvedValueOnce(
      new Response("requires maintainer", { status: 403 }),
    );
    const res = await freezeEnvironment({
      slug: "acme",
      name: "production",
      reason: "x",
    });
    expect(res).toEqual({ ok: false, error: "requires maintainer" });
  });
});

describe("unfreezeEnvironment", () => {
  it("DELETEs the same endpoint with no body", async () => {
    const res = await unfreezeEnvironment({ slug: "acme", name: "production" });
    expect(res.ok).toBe(true);
    const [url, init] = call();
    expect(url).toBe(
      "http://api.test/api/v1/projects/acme/environment-freezes/production",
    );
    expect(init.method).toBe("DELETE");
    expect(init.body).toBeUndefined();
  });

  it("rejects an invalid name without calling the API", async () => {
    const res = await unfreezeEnvironment({ slug: "acme", name: "prod/uction" });
    expect(res.ok).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
