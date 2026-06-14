import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RollbackButton } from "./rollback-button.client";
import { rollbackEnvironment } from "@/server/actions/environments";

const refresh = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh }) }));
vi.mock("@/server/actions/environments", () => ({
  rollbackEnvironment: vi.fn(),
}));
const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({
  toast: { success: (m: string) => toastSuccess(m), error: (m: string) => toastError(m) },
}));

const props = {
  slug: "acme",
  environmentId: "env-1",
  environmentName: "production",
  revisionId: "rev-40",
  version: "1.40.old",
};

afterEach(() => {
  vi.clearAllMocks();
});

describe("RollbackButton", () => {
  it("confirms then calls the rollback action with the target revision", async () => {
    vi.mocked(rollbackEnvironment).mockResolvedValue({ ok: true });
    render(<RollbackButton {...props} />);

    // Opening the dialog does NOT fire the action.
    fireEvent.click(
      screen.getByRole("button", { name: /Roll back production to 1.40.old/ }),
    );
    expect(rollbackEnvironment).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Roll back" }));

    await waitFor(() => expect(rollbackEnvironment).toHaveBeenCalledTimes(1));
    expect(rollbackEnvironment).toHaveBeenCalledWith({
      slug: "acme",
      environmentId: "env-1",
      toRevisionId: "rev-40",
    });
    await waitFor(() => expect(toastSuccess).toHaveBeenCalled());
    expect(refresh).toHaveBeenCalled();
  });

  it("surfaces a server error as a toast and does not refresh", async () => {
    vi.mocked(rollbackEnvironment).mockResolvedValue({
      ok: false,
      error: "server 409: deploy job is still active",
    });
    render(<RollbackButton {...props} />);
    fireEvent.click(
      screen.getByRole("button", { name: /Roll back production to 1.40.old/ }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Roll back" }));

    await waitFor(() => expect(toastError).toHaveBeenCalled());
    expect(refresh).not.toHaveBeenCalled();
  });
});
