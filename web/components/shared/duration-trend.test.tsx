import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DurationTrend, runDurationPoints } from "./duration-trend";
import type { RunSummary } from "@/types/api";

function run(p: Partial<RunSummary> & { counter: number }): RunSummary {
  return {
    id: `r${p.counter}`,
    pipeline_id: "p1",
    pipeline_name: "build",
    cause: "push",
    status: "success",
    has_services: false,
    service_names: [],
    created_at: "2026-01-01T00:00:00Z",
    ...p,
  };
}

describe("runDurationPoints", () => {
  it("orders by finish time (not counter), drops unfinished runs", () => {
    // counter order ≠ finish order — counter is unique only per pipeline.
    const runs = [
      run({ counter: 3, started_at: "2026-01-01T00:00:00Z", finished_at: "2026-01-01T00:00:30Z" }),
      run({ counter: 1, started_at: "2026-01-01T00:00:00Z", finished_at: "2026-01-01T00:01:00Z" }),
      run({ counter: 2, started_at: "2026-01-01T00:00:00Z" }), // running — dropped
    ];
    const pts = runDurationPoints(runs);
    // #3 finished first (00:30) then #1 (01:00) → oldest→newest by finish time.
    expect(pts.map((p) => p.label)).toEqual(["#3", "#1"]);
    expect(pts.map((p) => p.durationSeconds)).toEqual([30, 60]);
  });

  it("labels with the pipeline name when aggregating across pipelines", () => {
    const runs = [
      run({ counter: 7, pipeline_name: "build", started_at: "2026-01-01T00:00:00Z", finished_at: "2026-01-01T00:00:10Z" }),
      run({ counter: 7, pipeline_name: "lint", started_at: "2026-01-01T00:00:00Z", finished_at: "2026-01-01T00:00:20Z" }),
    ];
    expect(runDurationPoints(runs, 30, { withPipeline: true }).map((p) => p.label)).toEqual([
      "build #7",
      "lint #7",
    ]);
  });

  it("caps at the most recent N by finish time", () => {
    const runs = Array.from({ length: 40 }, (_, i) =>
      run({
        counter: i + 1,
        started_at: "2026-01-01T00:00:00Z",
        // finish time increases with counter so time-order == counter-order here.
        finished_at: new Date(Date.UTC(2026, 0, 1, 0, 0, 10 + i)).toISOString(),
      }),
    );
    const pts = runDurationPoints(runs, 30);
    expect(pts).toHaveLength(30);
    expect(pts[0]!.label).toBe("#11");
    expect(pts[29]!.label).toBe("#40");
  });
});

describe("DurationTrend", () => {
  it("shows a hint below 2 finished runs", () => {
    render(<DurationTrend points={[{ label: "#1", durationSeconds: 10, status: "success" }]} />);
    expect(screen.getByText(/not enough finished runs/i)).toBeTruthy();
  });

  it("renders bars + a median with enough data", () => {
    render(
      <DurationTrend
        points={[
          { label: "#1", durationSeconds: 10, status: "success" },
          { label: "#2", durationSeconds: 30, status: "failed" },
          { label: "#3", durationSeconds: 20, status: "success" },
        ]}
      />,
    );
    expect(screen.getByText("3 runs")).toBeTruthy();
    expect(screen.getByText(/median/i)).toBeTruthy();
    // one titled bar per point
    expect(screen.getByTitle(/#2 · .* · failed/)).toBeTruthy();
  });
});
