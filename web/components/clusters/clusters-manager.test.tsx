import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { ClustersManager } from "./clusters-manager.client";
import { CREDENTIAL_PRESERVE_SENTINEL } from "@/lib/clusters";
import type { AdminCluster } from "@/server/queries/admin";

// Server actions are mocked at module level — the manager dispatches
// them on save / delete; a unit test must never fire a real fetch.
// Each mock takes the action's single input arg so `.mock.calls[i][0]`
// is the dispatched payload we assert on.
const createCluster = vi.fn(async (_input: Record<string, unknown>) => ({
  ok: true as const,
}));
const updateCluster = vi.fn(async (_input: Record<string, unknown>) => ({
  ok: true as const,
}));
const deleteCluster = vi.fn(async (_input: Record<string, unknown>) => ({
  ok: true as const,
}));
const testCluster = vi.fn(async (_input: Record<string, unknown>) => ({
  ok: true as const,
  probe: { status: "ok", message: "connected — Kubernetes v1.30.0" },
}));

vi.mock("@/server/actions/clusters", () => ({
  createCluster: (input: Record<string, unknown>) => createCluster(input),
  updateCluster: (input: Record<string, unknown>) => updateCluster(input),
  deleteCluster: (input: Record<string, unknown>) => deleteCluster(input),
  testCluster: (input: Record<string, unknown>) => testCluster(input),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}));

const TOKEN_CA = "-----BEGIN CERTIFICATE-----\nMOCKCA==\n-----END CERTIFICATE-----";

const sample: AdminCluster[] = [
  {
    id: "c1",
    name: "prod-us-east",
    description: "production east",
    auth_type: "token",
    api_server: "https://10.0.0.1:6443",
    has_ca: true,
    ca_cert: TOKEN_CA,
    allowed_projects: ["proj-1"],
    created_at: "2026-06-01T12:00:00Z",
    updated_at: "2026-06-01T12:00:00Z",
  },
  {
    id: "c2",
    name: "staging",
    description: "",
    auth_type: "in_cluster",
    api_server: "",
    has_ca: false,
    ca_cert: "",
    allowed_projects: [],
    created_at: "2026-06-02T12:00:00Z",
    updated_at: "2026-06-02T12:00:00Z",
  },
];

const projects = [
  { id: "proj-1", name: "Billing", slug: "billing" },
  { id: "proj-2", name: "Web", slug: "web" },
];

describe("ClustersManager", () => {
  it("renders one row per cluster with auth_type + api_server + project count", () => {
    render(<ClustersManager initial={sample} projects={projects} />);
    expect(screen.getByText("prod-us-east")).toBeTruthy();
    expect(screen.getByText("https://10.0.0.1:6443")).toBeTruthy();
    // token row shows the "token" auth badge.
    expect(screen.getByText("token")).toBeTruthy();
    // in-cluster row: badge label + "—" api server.
    expect(screen.getByText("in-cluster")).toBeTruthy();
    // staging has an empty allow-list → shows "all".
    expect(screen.getByText("all")).toBeTruthy();
  });

  it("shows the empty hint when no clusters exist", () => {
    render(<ClustersManager initial={[]} projects={projects} />);
    expect(screen.getByText(/No clusters registered yet/i)).toBeTruthy();
  });

  it("auth_type select toggles which credential fields render", () => {
    render(<ClustersManager initial={[]} projects={projects} />);
    fireEvent.click(screen.getByRole("button", { name: /new cluster/i }));

    const authSelect = screen.getByLabelText("Auth type") as HTMLSelectElement;

    // Default is kubeconfig → a single kubeconfig textarea, no api server.
    expect(screen.getByLabelText("Kubeconfig")).toBeTruthy();
    expect(screen.queryByLabelText("API server")).toBeNull();
    expect(screen.queryByLabelText("Bearer token")).toBeNull();

    // token → api server + CA cert + bearer token, no kubeconfig. The
    // "API server" label carries a "*" on create (required), so match
    // by regex rather than the exact string.
    fireEvent.change(authSelect, { target: { value: "token" } });
    expect(screen.getByLabelText(/API server/)).toBeTruthy();
    // CA certificate is required (carries a "*") → match by regex.
    expect(screen.getByLabelText(/CA certificate/)).toBeTruthy();
    expect(screen.getByLabelText("Bearer token")).toBeTruthy();
    expect(screen.queryByLabelText("Kubeconfig")).toBeNull();

    // in_cluster → no credential fields, an explanatory note instead.
    fireEvent.change(authSelect, { target: { value: "in_cluster" } });
    expect(screen.queryByLabelText("Kubeconfig")).toBeNull();
    expect(screen.queryByLabelText(/API server/)).toBeNull();
    expect(screen.getByText(/in-cluster ServiceAccount/i)).toBeTruthy();
  });

  it("sends the preserve sentinel when editing without re-entering the credential", async () => {
    render(<ClustersManager initial={sample} projects={projects} />);

    fireEvent.click(screen.getByRole("button", { name: /edit prod-us-east/i }));
    // Editing a token cluster — credential (bearer token) field starts
    // blank because the server never echoes it back.
    const token = screen.getByLabelText("Bearer token") as HTMLTextAreaElement;
    expect(token.value).toBe("");

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    // Action dispatch is async (startTransition); flush microtasks.
    await Promise.resolve();
    await Promise.resolve();

    expect(updateCluster).toHaveBeenCalledTimes(1);
    const arg = updateCluster.mock.calls[0]![0];
    expect(arg.id).toBe("c1");
    expect(arg.credential).toBe(CREDENTIAL_PRESERVE_SENTINEL);
    expect(arg.auth_type).toBe("token");
    // HIGH 3: the public CA cert is prefilled and re-sent on a metadata-
    // only edit, so the server doesn't reject the (now CA-mandatory)
    // token cluster.
    expect(arg.ca_cert).toBe(TOKEN_CA);
  });

  it("prefills the CA cert on edit (it is public, not write-only)", () => {
    render(<ClustersManager initial={sample} projects={projects} />);
    fireEvent.click(screen.getByRole("button", { name: /edit prod-us-east/i }));
    const ca = screen.getByLabelText(/CA certificate/) as HTMLTextAreaElement;
    expect(ca.value).toBe(TOKEN_CA);
    // The bearer token, by contrast, stays blank (write-only secret).
    const token = screen.getByLabelText("Bearer token") as HTMLTextAreaElement;
    expect(token.value).toBe("");
  });

  it("sends the typed credential verbatim on create (no sentinel)", async () => {
    render(<ClustersManager initial={[]} projects={projects} />);
    fireEvent.click(screen.getByRole("button", { name: /new cluster/i }));

    fireEvent.change(screen.getByLabelText("Auth type"), {
      target: { value: "kubeconfig" },
    });
    fireEvent.change(screen.getByLabelText(/^Name/i), {
      target: { value: "new-cluster" },
    });
    fireEvent.change(screen.getByLabelText("Kubeconfig"), {
      target: { value: "apiVersion: v1" },
    });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await Promise.resolve();
    await Promise.resolve();

    expect(createCluster).toHaveBeenCalledTimes(1);
    const arg = createCluster.mock.calls[0]![0];
    expect(arg.name).toBe("new-cluster");
    expect(arg.credential).toBe("apiVersion: v1");
  });

  it("delete asks for confirmation and surfaces it before dispatching", () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    render(<ClustersManager initial={sample} projects={projects} />);

    fireEvent.click(screen.getByRole("button", { name: /delete prod-us-east/i }));
    expect(confirmSpy).toHaveBeenCalledWith(
      expect.stringContaining(`Delete cluster "prod-us-east"`),
    );
    expect(deleteCluster).not.toHaveBeenCalled();
    confirmSpy.mockRestore();
  });

  it("allow-list checkboxes toggle and persist the selected project id", async () => {
    render(<ClustersManager initial={[]} projects={projects} />);
    fireEvent.click(screen.getByRole("button", { name: /new cluster/i }));

    fireEvent.change(screen.getByLabelText(/^Name/i), {
      target: { value: "c3" },
    });
    // in_cluster avoids the credential-required path so create dispatches.
    fireEvent.change(screen.getByLabelText("Auth type"), {
      target: { value: "in_cluster" },
    });

    const webRow = screen.getByLabelText("Allow Web") as HTMLInputElement;
    expect(webRow.checked).toBe(false);
    fireEvent.click(webRow);
    expect((screen.getByLabelText("Allow Web") as HTMLInputElement).checked).toBe(
      true,
    );

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await Promise.resolve();
    await Promise.resolve();

    expect(createCluster).toHaveBeenCalled();
    const arg = createCluster.mock.calls.at(-1)![0];
    expect(arg.allowed_projects).toEqual(["proj-2"]);
  });

  it("the test-connection button probes the cluster by id", async () => {
    const { toast } = await import("sonner");
    render(<ClustersManager initial={sample} projects={projects} />);

    fireEvent.click(
      screen.getByRole("button", { name: /test connection prod-us-east/i }),
    );
    await Promise.resolve();
    await Promise.resolve();

    expect(testCluster).toHaveBeenCalledTimes(1);
    expect(testCluster.mock.calls[0]![0]).toEqual({ id: "c1" });
    // status "ok" → success toast carrying the probe message.
    expect(toast.success).toHaveBeenCalledWith(
      expect.stringContaining("Kubernetes v1.30.0"),
    );
  });
});
