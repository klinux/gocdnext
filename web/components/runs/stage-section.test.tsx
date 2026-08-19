import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { StageSection } from "./stage-section";
import { TooltipProvider } from "@/components/ui/tooltip";
import type { StageDetail } from "@/types/api";

vi.mock("@/server/actions/approvals", () => ({
  approveJob: vi.fn(async () => ({ ok: true })),
  rejectJob: vi.fn(async () => ({ ok: true })),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

describe("StageSection — freeze presentation", () => {
  it("shows a freeze-held approval stage as Freezing with elapsed time paused", () => {
    const stage: StageDetail = {
      id: "s1",
      name: "deploy",
      ordinal: 0,
      status: "running",
      started_at: "2026-08-19T10:00:00Z",
      jobs: [
        {
          id: "j1",
          stage_run_id: "s1",
          name: "approve",
          status: "awaiting_approval",
          attempt: 0,
          approval_gate: true,
          held_by_freeze: true,
          frozen_envs: ["production"],
        },
      ],
    };

    render(
      <TooltipProvider>
        <StageSection stage={stage} runID="r1" />
      </TooltipProvider>,
    );

    expect(screen.getAllByText("Freezing")).toHaveLength(2);
    expect(screen.getAllByText("paused")).toHaveLength(2);
  });
});
