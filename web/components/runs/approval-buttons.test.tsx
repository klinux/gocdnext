import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ApprovalButtons } from "./approval-buttons.client";
import { TooltipProvider } from "@/components/ui/tooltip";

vi.mock("@/server/actions/approvals", () => ({
  approveJob: vi.fn(async () => ({ ok: true })),
  rejectJob: vi.fn(async () => ({ ok: true })),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

afterEach(() => vi.clearAllMocks());

const base = { jobRunID: "j1", runID: "r1", jobName: "deploy" };

function renderButtons(props: Record<string, unknown>) {
  return render(
    <TooltipProvider>
      <ApprovalButtons {...base} {...props} />
    </TooltipProvider>,
  );
}

describe("ApprovalButtons — freeze hold (#227)", () => {
  it("disables the Approve trigger when held from the start; Reject stays enabled", () => {
    renderButtons({ heldByFreeze: true, frozenEnvs: ["production"] });
    expect(
      (screen.getByRole("button", { name: /Approve/i }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    expect(
      (screen.getByRole("button", { name: /Reject/i }) as HTMLButtonElement)
        .disabled,
    ).toBe(false);
  });

  // Review #2: a dialog opened BEFORE the freeze-detecting poll must not still
  // confirm into a 409. Open the approve dialog (unfrozen), then a re-render
  // flips heldByFreeze true — the dialog stays open and its confirm goes disabled.
  it("disables an already-open approve dialog's confirm when a freeze arrives", async () => {
    const { rerender } = renderButtons({ heldByFreeze: false });

    fireEvent.click(screen.getByRole("button", { name: /Approve/i }));
    const dialog = await screen.findByRole("dialog");
    // confirm is live before the freeze
    expect(
      (within(dialog).getByRole("button", { name: /^Approve$/i }) as HTMLButtonElement)
        .disabled,
    ).toBe(false);

    rerender(
      <TooltipProvider>
        <ApprovalButtons {...base} heldByFreeze frozenEnvs={["production"]} />
      </TooltipProvider>,
    );

    const stillOpen = screen.getByRole("dialog");
    expect(within(stillOpen).getByText(/Approval is paused/i)).toBeTruthy();
    expect(
      (within(stillOpen).getByRole("button", { name: /^Approve$/i }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
  });
});
