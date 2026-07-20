import { afterEach, describe, expect, it, vi } from "vitest";

// The query forwards the session cookie and reads the API base from env; stub both so
// the test exercises only the fetch/status handling.
vi.mock("next/headers", () => ({
  cookies: async () => ({ get: () => ({ value: "sess" }) }),
}));
vi.mock("@/lib/env", () => ({
  env: { GOCDNEXT_API_URL: "http://api.test", GOCDNEXT_PUBLIC_API_URL: "" },
}));

import { GocdnextAPIError, listDeployTargets } from "./projects";

afterEach(() => {
  vi.restoreAllMocks();
});

const okResponse = (body: unknown) => ({
  ok: true,
  status: 200,
  json: async () => body,
});
const errResponse = (status: number) => ({
  ok: false,
  status,
  text: async () => `status ${status}`,
});

describe("listDeployTargets", () => {
  it("returns the registered targets on 200 (maintainer)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        okResponse({
          deploy_targets: [
            {
              environment: "production",
              provider: "argocd",
              cluster: "prod-gke",
              application: "checkout",
              namespace: "argocd",
              sync_mode: "trigger",
            },
          ],
        }),
      ),
    );
    const res = await listDeployTargets("acme");
    expect(res.deploy_targets).toHaveLength(1);
    expect(res.deploy_targets[0]?.application).toBe("checkout");
  });

  // Security: the endpoint is maintainer-gated. A viewer (403) must NOT error the
  // Environments page — the config just stays hidden.
  it.each([401, 403, 501])(
    "tolerates a %d and returns no targets (viewer / registrar off)",
    async (status) => {
      vi.stubGlobal("fetch", vi.fn().mockResolvedValue(errResponse(status)));
      expect((await listDeployTargets("acme")).deploy_targets).toEqual([]);
    },
  );

  it("propagates a real server error (500)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(errResponse(500)));
    await expect(listDeployTargets("acme")).rejects.toBeInstanceOf(GocdnextAPIError);
  });
});
