import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { JobActions } from "./job-actions.client";
import { TooltipProvider } from "@/components/ui/tooltip";
import type { MergedJob } from "@/components/pipelines/pipeline-card-helpers";

const approveJob = vi.fn(async (_i: unknown) => ({ ok: true as const }));
vi.mock("@/server/actions/approvals", () => ({
  approveJob: (i: unknown) => approveJob(i),
  rejectJob: vi.fn(async () => ({ ok: true })),
}));
vi.mock("@/server/actions/runs", () => ({
  cancelJob: vi.fn(),
  cancelRun: vi.fn(),
  rerunJob: vi.fn(),
  rerunRun: vi.fn(),
}));
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: vi.fn() }) }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

afterEach(() => vi.clearAllMocks());

function gate(overrides: Partial<MergedJob["run"]> = {}): MergedJob {
  return {
    key: "gate",
    name: "gate",
    approvalGate: true,
    run: { id: "jr1", name: "gate", status: "awaiting_approval", ...overrides },
  };
}

function renderActions(job: MergedJob) {
  return render(
    <TooltipProvider>
      <JobActions job={job} runId="r1" tooltip="gate · awaiting_approval">
        <span>node</span>
      </JobActions>
    </TooltipProvider>,
  );
}

function openMenu() {
  fireEvent.click(screen.getByRole("button", { name: /Actions for gate/i }));
}

describe("JobActions — freeze hold (#227)", () => {
  it("held: Approve disabled + on-hold note (names both envs); Reject enabled", () => {
    renderActions(gate({ held_by_freeze: true, frozen_envs: ["prod", "staging"] }));
    openMenu();

    const menu = screen.getByRole("menu");
    const approve = within(menu).getByRole("menuitem", { name: /Approve/i });
    expect(approve.getAttribute("aria-disabled")).toBe("true");
    const reject = within(menu).getByRole("menuitem", { name: /Reject/i });
    expect(reject.getAttribute("aria-disabled")).not.toBe("true");

    expect(within(menu).getByText(/On hold/i)).toBeTruthy();
    // both env names stay accessible (compact phrase collapses to a count, the
    // full list is rendered beside it).
    expect(within(menu).getByText(/prod, staging/)).toBeTruthy();
  });

  it("not held: Approve enabled, no note", () => {
    renderActions(gate());
    openMenu();
    const menu = screen.getByRole("menu");
    expect(
      within(menu).getByRole("menuitem", { name: /Approve/i }).getAttribute("aria-disabled"),
    ).not.toBe("true");
    expect(within(menu).queryByText(/On hold/i)).toBeNull();
  });

  it("guards onApprove: a click while held never calls the server action", () => {
    renderActions(gate({ held_by_freeze: true, frozen_envs: ["prod"] }));
    openMenu();
    // Even if the disabled item is force-clicked, the early-return guard holds.
    fireEvent.click(screen.getByRole("menuitem", { name: /Approve/i }));
    expect(approveJob).not.toHaveBeenCalled();
  });

  // Review #4: a menu opened BEFORE the freeze poll must react live — Approve
  // goes disabled + note appears without closing; unfreeze re-enables it.
  it("reacts to a freeze arriving while the menu is open, then to unfreeze", () => {
    const { rerender } = renderActions(gate());
    openMenu();
    expect(
      screen.getByRole("menuitem", { name: /Approve/i }).getAttribute("aria-disabled"),
    ).not.toBe("true");

    rerender(
      <TooltipProvider>
        <JobActions job={gate({ held_by_freeze: true, frozen_envs: ["prod"] })} runId="r1" tooltip="t">
          <span>node</span>
        </JobActions>
      </TooltipProvider>,
    );
    // menu stayed open, now reflects the freeze
    expect(screen.getByRole("menu")).toBeTruthy();
    expect(screen.getByText(/On hold/i)).toBeTruthy();
    expect(
      screen.getByRole("menuitem", { name: /Approve/i }).getAttribute("aria-disabled"),
    ).toBe("true");

    rerender(
      <TooltipProvider>
        <JobActions job={gate()} runId="r1" tooltip="t">
          <span>node</span>
        </JobActions>
      </TooltipProvider>,
    );
    expect(
      screen.getByRole("menuitem", { name: /Approve/i }).getAttribute("aria-disabled"),
    ).not.toBe("true");
  });
});
