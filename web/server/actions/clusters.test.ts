import { beforeEach, describe, expect, it, vi } from "vitest";

// The action is a "use server" module: it reads the session cookie and the API base,
// then POST/PUTs JSON. Stub the Next-only bits so we can assert the BODY that actually
// reaches the API — a schema that parses is not proof the field survives the round-trip.
vi.mock("next/headers", () => ({
  cookies: async () => ({ get: () => ({ value: "session-token" }) }),
}));
vi.mock("next/cache", () => ({ revalidatePath: vi.fn() }));
vi.mock("@/lib/env", () => ({ env: { GOCDNEXT_API_URL: "http://api.test" } }));

import { createCluster, updateCluster } from "./clusters";

const fetchMock = vi.fn(async () => new Response(null, { status: 201 }));
beforeEach(() => {
  fetchMock.mockClear();
  vi.stubGlobal("fetch", fetchMock);
});

function sentBody(): Record<string, unknown> {
  const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  return JSON.parse(String(init.body)) as Record<string, unknown>;
}

// ClusterWriteInput is the Zod OUTPUT type (defaults applied), so every field is
// required here even though the schema marks most optional.
const base = {
  name: "prod-gke",
  description: "",
  auth_type: "kubeconfig" as const,
  api_server: "",
  ca_cert: "",
  credential: "apiVersion: v1\nkind: Config\n",
  allowed_projects: ["proj-a"],
};

describe("cluster write actions", () => {
  // Regression: the field was absent from writeSchema, so Zod stripped it before the
  // fetch. The form appeared to save the toggle and the API never saw it.
  it("forwards allow_declarative_targets=true to the API", async () => {
    await createCluster({ ...base, allow_declarative_targets: true });
    expect(sentBody().allow_declarative_targets).toBe(true);
  });

  it("forwards an explicit false (disabling must reach the API too)", async () => {
    await updateCluster({ ...base, id: "c-1", allow_declarative_targets: false });
    expect(sentBody().allow_declarative_targets).toBe(false);
  });

  it("keeps sending the fields it already carried", async () => {
    await createCluster({ ...base, allow_declarative_targets: true });
    const body = sentBody();
    expect(body.name).toBe("prod-gke");
    expect(body.allowed_projects).toEqual(["proj-a"]);
  });
});
