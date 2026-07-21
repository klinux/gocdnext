import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RemoveEnvironment } from "./remove-environment-button.client";

const deleteEnvironment = vi.fn(async (_input: unknown) => ({
  ok: true as const,
}));
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: vi.fn() }) }));
vi.mock("@/server/actions/environments", () => ({
  deleteEnvironment: (input: unknown) => deleteEnvironment(input),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

afterEach(() => vi.clearAllMocks());

describe("RemoveEnvironment", () => {
  it("confirms before deleting and calls the action with the env id", async () => {
    render(
      <RemoveEnvironment
        slug="acme"
        environmentId="env-1"
        environmentName="production"
      />,
    );
    // The first click only reveals the confirm — no destructive call yet.
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(deleteEnvironment).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
    await waitFor(() =>
      expect(deleteEnvironment).toHaveBeenCalledWith({
        slug: "acme",
        environmentId: "env-1",
      }),
    );
  });

  it("cancels without deleting", () => {
    render(
      <RemoveEnvironment
        slug="acme"
        environmentId="env-1"
        environmentName="production"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(deleteEnvironment).not.toHaveBeenCalled();
    // Back to the initial affordance.
    expect(screen.getByRole("button", { name: "Remove" })).toBeTruthy();
  });
});
