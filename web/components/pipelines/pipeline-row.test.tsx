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

describe("PipelineRow — stage p95", () => {
  it("shows the p95 duration next to the stage", () => {
    const pipeline = pipelineWith(false);
    pipeline.latest_run_stages = [
      ...(pipeline.latest_run_stages ?? []),
      {
        id: "s2",
        name: "publish",
        ordinal: 1,
        status: "success",
        jobs: [],
      },
    ];
    pipeline.metrics = {
      window_days: 7,
      runs_considered: 42,
      success_rate: 0.96,
      lead_time_p50_seconds: 1080,
      process_time_p50_seconds: 780,
      stage_stats: [
        {
          name: "approve",
          runs_considered: 42,
          success_rate: 0.96,
          duration_p50_seconds: 360,
          duration_p95_seconds: 521,
        },
        {
          name: "publish",
          runs_considered: 42,
          success_rate: 0.98,
          duration_p50_seconds: 90,
          duration_p95_seconds: 120,
        },
      ],
    };

    const view = render(
      <TooltipProvider>
        <PipelineRow
          projectSlug="acme"
          pipeline={pipeline}
          edges={[]}
          runs={[]}
          showRail={false}
        />
      </TooltipProvider>,
    );

    expect(view.getAllByText("p95")).toHaveLength(2);
    const slowest = view.getByText("8m 41s").parentElement;
    expect(slowest).toBeTruthy();
    expect(slowest?.className).toContain("bg-amber-500/10");
  });
});
