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
  it("computes seconds, drops unfinished runs, orders oldest→newest", () => {
    const runs = [
      run({ counter: 3, started_at: "2026-01-01T00:00:00Z", finished_at: "2026-01-01T00:00:30Z" }),
      run({ counter: 1, started_at: "2026-01-01T00:00:00Z", finished_at: "2026-01-01T00:01:00Z" }),
      run({ counter: 2, started_at: "2026-01-01T00:00:00Z" }), // running — no finished_at
    ];
    const pts = runDurationPoints(runs);
    expect(pts.map((p) => p.label)).toEqual(["#1", "#3"]); // #2 dropped, sorted asc
    expect(pts.map((p) => p.durationSeconds)).toEqual([60, 30]);
  });

  it("caps at the limit (most recent N)", () => {
    const runs = Array.from({ length: 40 }, (_, i) =>
      run({ counter: i + 1, started_at: "2026-01-01T00:00:00Z", finished_at: "2026-01-01T00:00:10Z" }),
    );
    const pts = runDurationPoints(runs, 30);
    expect(pts).toHaveLength(30);
    expect(pts[0]!.label).toBe("#11"); // oldest kept is 40-30+1
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
