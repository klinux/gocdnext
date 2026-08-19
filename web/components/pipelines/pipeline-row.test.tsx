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
    definition_jobs: [{ name: "gate", stage: "approve", approval_gate: true }],
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

function renderPipeline(pipeline: PipelineSummary) {
  return render(
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
  it("omits gate wait time and highlights only the slowest comparable stage", () => {
    const pipeline = pipelineWith(false);
    pipeline.definition_jobs?.push(
      { name: "compile", stage: "build" },
      { name: "publish", stage: "publish" },
    );
    pipeline.latest_run_stages = [
      ...(pipeline.latest_run_stages ?? []),
      {
        id: "s2",
        name: "build",
        ordinal: 1,
        status: "success",
        jobs: [],
      },
      {
        id: "s3",
        name: "publish",
        ordinal: 2,
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
          name: "build",
          runs_considered: 42,
          success_rate: 0.98,
          duration_p50_seconds: 120,
          duration_p95_seconds: 180,
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

    const view = renderPipeline(pipeline);

    expect(view.getAllByText("p95")).toHaveLength(2);
    expect(view.queryByText("8m 41s")).toBeNull();
    expect(view.getByText("3m 0s").parentElement?.className).toContain(
      "bg-amber-500/10",
    );
  });

  it("hides the badge when a stage has no valid p95 sample", () => {
    const pipeline = pipelineWith(false);
    pipeline.latest_run_stages?.push({
      id: "s2",
      name: "publish",
      ordinal: 1,
      status: "success",
      jobs: [],
    });
    pipeline.metrics = {
      window_days: 7,
      runs_considered: 1,
      success_rate: 1,
      lead_time_p50_seconds: 10,
      process_time_p50_seconds: 10,
      stage_stats: [
        {
          name: "approve",
          runs_considered: 0,
          success_rate: 0,
          duration_p50_seconds: 0,
          duration_p95_seconds: 0,
        },
      ],
    };

    expect(renderPipeline(pipeline).queryByText("p95")).toBeNull();
  });

  it("highlights every comparable stage tied for the slowest p95", () => {
    const pipeline = pipelineWith(false);
    pipeline.definition_stages = ["build", "publish"];
    pipeline.definition_jobs = [
      { name: "compile", stage: "build" },
      { name: "publish", stage: "publish" },
    ];
    pipeline.latest_run_stages = [
      { id: "s1", name: "build", ordinal: 0, status: "success", jobs: [] },
      { id: "s2", name: "publish", ordinal: 1, status: "success", jobs: [] },
    ];
    pipeline.metrics = {
      window_days: 7,
      runs_considered: 42,
      success_rate: 1,
      lead_time_p50_seconds: 240,
      process_time_p50_seconds: 240,
      stage_stats: ["build", "publish"].map((name) => ({
        name,
        runs_considered: 42,
        success_rate: 1,
        duration_p50_seconds: 90,
        duration_p95_seconds: 120,
      })),
    };

    const badges = renderPipeline(pipeline)
      .getAllByText("2m 0s")
      .map((duration) => duration.parentElement);
    expect(badges).toHaveLength(2);
    expect(badges.every((badge) => badge?.className.includes("bg-amber-500/10"))).toBe(
      true,
    );
  });
});
