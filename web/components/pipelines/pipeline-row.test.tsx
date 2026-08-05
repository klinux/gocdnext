import { render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { PipelineRow } from "./pipeline-row";
import { TooltipProvider } from "@/components/ui/tooltip";
import type { PipelineSummary, RunSummary } from "@/types/api";

vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: vi.fn() }) }));
vi.mock("@/server/actions/approvals", () => ({ approveJob: vi.fn(), rejectJob: vi.fn() }));
vi.mock("@/server/actions/runs", () => ({
  cancelJob: vi.fn(),
  cancelRun: vi.fn(),
  rerunJob: vi.fn(),
  rerunRun: vi.fn(),
}));
vi.mock("@/server/actions/pipelines", () => ({ triggerPipeline: vi.fn() }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

// Minimal latest run for the awaiting gate node.
const latestRun = {
  id: "r1",
  pipeline_id: "p1",
  counter: 1,
  cause: "webhook",
  status: "awaiting_approval",
  created_at: "2026-08-05T00:00:00Z",
} as RunSummary; // test fixture — PipelineRow reads only these scalar fields for the node.

function pipelineWith(held: boolean): PipelineSummary {
  return {
    id: "p1",
    name: "release",
    definition_version: 1,
    updated_at: "2026-08-05T00:00:00Z",
    latest_run: latestRun,
    latest_run_stages: [
      {
        id: "s1",
        name: "approve",
        ordinal: 0,
        status: "awaiting_approval",
        jobs: [
          {
            id: "jr1",
            name: "gate",
            status: "awaiting_approval",
            held_by_freeze: held || undefined,
            frozen_envs: held ? ["prod"] : undefined,
          },
        ],
      },
    ],
  };
}

function renderRow(held: boolean) {
  return render(
    <TooltipProvider>
      <PipelineRow
        projectSlug="acme"
        pipeline={pipelineWith(held)}
        edges={[]}
        runs={[]}
        showRail={false}
      />
    </TooltipProvider>,
  );
}

describe("PipelineRow — frozen APPROVE node (#227)", () => {
  it("renders a snowflake on the awaiting gate node when held by freeze", () => {
    const { container } = renderRow(true);
    expect(container.querySelector(".lucide-snowflake")).toBeTruthy();
  });

  it("no snowflake on a plain awaiting gate", () => {
    const { container } = renderRow(false);
    expect(container.querySelector(".lucide-snowflake")).toBeNull();
  });
});
