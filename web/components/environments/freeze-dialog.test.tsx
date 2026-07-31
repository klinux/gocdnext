import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { Button } from "@/components/ui/button";
import { FreezeDialog } from "./freeze-dialog.client";
import { freezeEnvironment } from "@/server/actions/environments";

vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: vi.fn() }) }));
vi.mock("@/server/actions/environments", () => ({
  freezeEnvironment: vi.fn(async () => ({ ok: true })),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const freezeMock = vi.mocked(freezeEnvironment);

afterEach(() => {
  vi.clearAllMocks();
});

// The trigger and the submit button both read "Freeze", so every assertion
// below is scoped INSIDE the dialog — otherwise a test could pass by matching
// the trigger and never exercise the form at all.
async function open(props: { environment?: string } = {}) {
  render(
    <FreezeDialog
      slug="acme"
      environment={props.environment}
      trigger={<Button>Open freeze dialog</Button>}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: "Open freeze dialog" }));
  return within(await screen.findByRole("dialog"));
}

describe("FreezeDialog", () => {
  it("refuses to submit without a reason", async () => {
    // The reason is what every operator hit by the block reads. A freeze with
    // no stated reason is the exact failure this feature exists to prevent, so
    // it must not be reachable through the UI at all.
    const dialog = await open({ environment: "production" });
    const submit = dialog.getByRole("button", { name: "Freeze" });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(submit);
    expect(freezeMock).not.toHaveBeenCalled();
  });

  it("submits the environment and reason once both are present", async () => {
    const dialog = await open({ environment: "production" });
    fireEvent.change(dialog.getByLabelText("Reason"), {
      target: { value: "month-end close" },
    });
    fireEvent.click(dialog.getByRole("button", { name: "Freeze" }));
    await waitFor(() =>
      expect(freezeMock).toHaveBeenCalledWith({
        slug: "acme",
        name: "production",
        reason: "month-end close",
      }),
    );
  });

  it("locks the name when freezing a specific environment", async () => {
    const dialog = await open({ environment: "production" });
    expect(dialog.getByLabelText("Reason")).toBeTruthy();
    expect(dialog.queryByLabelText("Environment")).toBeNull();
  });

  it("asks for a name in page-level (pre-emptive) mode", async () => {
    // Environments are created lazily at the first deploy, so freezing one that
    // has never shipped means naming it — there is no card to start from.
    const dialog = await open();
    fireEvent.change(dialog.getByLabelText("Environment"), {
      target: { value: "production" },
    });
    fireEvent.change(dialog.getByLabelText("Reason"), {
      target: { value: "PCI audit window" },
    });
    fireEvent.click(dialog.getByRole("button", { name: "Freeze" }));
    await waitFor(() =>
      expect(freezeMock).toHaveBeenCalledWith({
        slug: "acme",
        name: "production",
        reason: "PCI audit window",
      }),
    );
  });

  it("trims a padded environment name before submitting", async () => {
    const dialog = await open();
    fireEvent.change(dialog.getByLabelText("Environment"), {
      target: { value: "  production  " },
    });
    fireEvent.change(dialog.getByLabelText("Reason"), {
      target: { value: "incident INC-4412" },
    });
    fireEvent.click(dialog.getByRole("button", { name: "Freeze" }));
    await waitFor(() =>
      expect(freezeMock).toHaveBeenCalledWith(
        expect.objectContaining({ name: "production" }),
      ),
    );
  });
});
