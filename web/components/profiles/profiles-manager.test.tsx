import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { ProfilesManager } from "./profiles-manager.client";
import { createRunnerProfile, updateRunnerProfile } from "@/server/actions/runner-profiles";
import type { AdminRunnerProfile } from "@/server/queries/admin";

// Server actions are mocked at module level — the manager dispatches
// them on save / delete; we never want a real fetch from a unit test.
vi.mock("@/server/actions/runner-profiles", () => ({
  createRunnerProfile: vi.fn(async () => ({ ok: true })),
  updateRunnerProfile: vi.fn(async () => ({ ok: true })),
  deleteRunnerProfile: vi.fn(async () => ({ ok: true })),
}));

// `sonner` toasts noop in jsdom — the manager only reads `.success`
// / `.error` for side effect, the visual layer is irrelevant here.
vi.mock("sonner", () => ({
  toast: {
    success: vi.fn(),
    error: vi.fn(),
  },
}));

const sample: AdminRunnerProfile[] = [
  {
    id: "p1",
    name: "default",
    description: "vanilla pool",
    engine: "kubernetes",
    default_image: "alpine:3.20",
    default_cpu_request: "100m",
    default_cpu_limit: "1",
    default_mem_request: "256Mi",
    default_mem_limit: "1Gi",
    max_cpu: "4",
    max_mem: "8Gi",
    tags: ["linux", "amd64"], env: {}, secret_keys: [], secret_refs: {},
    node_selector: {}, tolerations: [], preferred_node_affinity: [],
    workspace_size: "", workspace_storage_class: "", dind_storage_size: "", dind_storage_class: "",
    created_at: "2026-04-27T12:00:00Z",
    updated_at: "2026-04-27T12:00:00Z",
  },
  {
    id: "p2",
    name: "gpu",
    description: "",
    engine: "kubernetes",
    default_image: "nvidia/cuda:12-runtime",
    default_cpu_request: "",
    default_cpu_limit: "",
    default_mem_request: "",
    default_mem_limit: "",
    max_cpu: "8",
    max_mem: "32Gi",
    tags: ["gpu"], env: {}, secret_keys: [], secret_refs: {},
    node_selector: {}, tolerations: [], preferred_node_affinity: [],
    workspace_size: "", workspace_storage_class: "", dind_storage_size: "", dind_storage_class: "",
    created_at: "2026-04-27T12:00:00Z",
    updated_at: "2026-04-27T12:00:00Z",
  },
];

describe("ProfilesManager", () => {
  it("renders one row per profile, with engine + tags + cap visible", () => {
    render(<ProfilesManager initial={sample} globalSecretNames={[]} />);
    expect(screen.getByText("default")).toBeTruthy();
    expect(screen.getByText("vanilla pool")).toBeTruthy();
    // "gpu" appears twice (profile name + tag chip on the gpu row);
    // grab both to assert the row was rendered.
    expect(screen.getAllByText("gpu").length).toBe(2);
    // The cap column reads "<cpu> / <mem>"
    expect(screen.getByText("4 / 8Gi")).toBeTruthy();
    expect(screen.getByText("8 / 32Gi")).toBeTruthy();
    expect(screen.getAllByText("kubernetes").length).toBeGreaterThan(0);
    expect(screen.getByText("linux")).toBeTruthy();
    expect(screen.getByText("amd64")).toBeTruthy();
  });

  it("shows the empty hint when no profiles exist", () => {
    render(<ProfilesManager initial={[]} globalSecretNames={[]} />);
    expect(screen.getByText(/No runner profiles yet/i)).toBeTruthy();
  });

  it("filters by name, description, or tag", () => {
    render(<ProfilesManager initial={sample} globalSecretNames={[]} />);
    const filter = screen.getByPlaceholderText(/Filter profiles/i);

    fireEvent.change(filter, { target: { value: "gpu" } });
    // After filtering only the gpu row remains: its profile name +
    // its tag chip = 2 instances, the default row is gone.
    expect(screen.queryByText("default")).toBeNull();
    expect(screen.getAllByText("gpu").length).toBe(2);

    // Filter on description hits "vanilla pool" → default profile only
    fireEvent.change(filter, { target: { value: "vanilla" } });
    expect(screen.getByText("default")).toBeTruthy();
    expect(screen.queryByText("nvidia/cuda:12-runtime")).toBeNull();

    // Filter on a tag matches the profile that carries it
    fireEvent.change(filter, { target: { value: "amd64" } });
    expect(screen.getByText("default")).toBeTruthy();
    // gpu row is filtered out — none of its tags or fields match "amd64".
    expect(screen.queryByText("nvidia/cuda:12-runtime")).toBeNull();
  });

  it("opens an empty form for new profile", () => {
    render(<ProfilesManager initial={sample} globalSecretNames={[]} />);

    fireEvent.click(screen.getByRole("button", { name: /new profile/i }));
    // Sheet title is the only heading-level "New profile" element;
    // disambiguate from the trigger button by querying via role.
    expect(screen.getByRole("heading", { name: /new profile/i })).toBeTruthy();
    // Name input is empty when creating.
    const sheetNameInput = screen.getByPlaceholderText("default") as HTMLInputElement;
    expect(sheetNameInput.value).toBe("");
  });

  it("renders the per-profile storage fields in the editor", () => {
    render(<ProfilesManager initial={sample} globalSecretNames={[]} />);
    fireEvent.click(screen.getByRole("button", { name: /new profile/i }));
    // Storage section heading + the four inputs (by placeholder) are present.
    expect(screen.getByText(/Storage \(isolated mode\)/i)).toBeTruthy();
    const dindSize = screen.getByPlaceholderText("e.g. 300Gi") as HTMLInputElement;
    expect(dindSize.value).toBe("");
    fireEvent.change(dindSize, { target: { value: "300Gi" } });
    expect(dindSize.value).toBe("300Gi");
    expect(screen.getByPlaceholderText("e.g. 100Gi")).toBeTruthy();
  });

  it("clone opens a create-mode editor pre-filled with a -copy name", () => {
    render(<ProfilesManager initial={sample} globalSecretNames={[]} />);
    fireEvent.click(screen.getByRole("button", { name: /clone default/i }));
    // Create mode (no id) → "New profile" heading, not "Edit profile".
    expect(screen.getByRole("heading", { name: /new profile/i })).toBeTruthy();
    // Name pre-filled from source + "-copy".
    const nameInput = screen.getByPlaceholderText("default") as HTMLInputElement;
    expect(nameInput.value).toBe("default-copy");
    // A non-secret field carried over (max_cpu from the "default" sample).
    const maxCpu = screen.getByDisplayValue("4") as HTMLInputElement;
    expect(maxCpu).toBeTruthy();
  });

  it("a freshly-created profile is editable with the server's real id (not a placeholder)", async () => {
    // Regression: creating a profile then editing it before a full refresh
    // must PUT to the real UUID the server returned, not a fabricated
    // "__opt__" id (which the server rejects as "invalid profile id").
    vi.mocked(createRunnerProfile).mockResolvedValueOnce({ ok: true, id: "real-uuid-9999" });
    render(<ProfilesManager initial={sample} globalSecretNames={[]} />);

    // Create a new profile.
    fireEvent.click(screen.getByRole("button", { name: /new profile/i }));
    fireEvent.change(screen.getByPlaceholderText("default"), { target: { value: "brand-new" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    // The new row appears once the create transition settles.
    await waitFor(() => expect(screen.getByText("brand-new")).toBeTruthy());

    // Edit it and save — the update action must receive the real id.
    fireEvent.click(screen.getByRole("button", { name: /edit brand-new/i }));
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() =>
      expect(vi.mocked(updateRunnerProfile)).toHaveBeenCalledWith(
        expect.objectContaining({ id: "real-uuid-9999" }),
      ),
    );
  });

  it("delete button asks for confirmation before dispatching", () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    render(<ProfilesManager initial={sample} globalSecretNames={[]} />);

    fireEvent.click(screen.getByRole("button", { name: /delete default/i }));
    expect(confirmSpy).toHaveBeenCalledWith(
      expect.stringContaining(`Delete profile "default"`),
    );
    confirmSpy.mockRestore();
  });
});
