import { beforeEach, describe, expect, it, vi } from "vitest";

// next/headers + next/cache only exist inside the Next runtime —
// stub them so the action is callable as a plain function.
vi.mock("next/headers", () => ({
  cookies: vi.fn(async () => ({
    get: () => ({ value: "session-token" }),
  })),
}));
vi.mock("next/cache", () => ({
  revalidatePath: vi.fn(),
}));
vi.mock("@/lib/env", () => ({
  env: { GOCDNEXT_API_URL: "http://server.test" },
}));

import { rotateOIDCKey } from "./oidc-keys";

const fetchMock = vi.fn();
vi.stubGlobal("fetch", fetchMock);

beforeEach(() => {
  fetchMock.mockReset();
});

describe("rotateOIDCKey", () => {
  it("rejects an unknown mode without touching the network", async () => {
    // A typo'd mode must never silently degrade to graceful — and a
    // client-side reject must not even reach the server.
    const res = await rotateOIDCKey({
      mode: "yolo" as unknown as "graceful",
    });
    expect(res.ok).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("POSTs the mode and forwards the session cookie", async () => {
    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({ kid: "fresh", mode: "emergency", note: "n" }),
        { status: 200 },
      ),
    );
    const res = await rotateOIDCKey({ mode: "emergency" });
    expect(res.ok).toBe(true);
    if (res.ok) expect(res.data.kid).toBe("fresh");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("http://server.test/api/v1/admin/oidc/keys/rotate");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({ mode: "emergency" });
    expect(
      (init.headers as Record<string, string>).Cookie,
    ).toContain("gocdnext_session=session-token");
  });

  it("maps a non-2xx response to ok:false with the server detail", async () => {
    fetchMock.mockResolvedValue(
      new Response("forbidden", { status: 403 }),
    );
    const res = await rotateOIDCKey({ mode: "graceful" });
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error).toMatch(/403/);
  });

  it("maps a thrown fetch error to ok:false, never throws", async () => {
    fetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    const res = await rotateOIDCKey({ mode: "graceful" });
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error).toMatch(/ECONNREFUSED/);
  });
});
