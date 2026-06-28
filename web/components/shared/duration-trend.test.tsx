import { describe, expect, it } from "vitest";

import { durationSummary, runDurationPoints, type DurationPoint } from "./duration-trend";
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

function pts(...durs: number[]): DurationPoint[] {
  return durs.map((d, i) => ({ label: `#${i + 1}`, durationSeconds: d, status: "success" }));
}

describe("runDurationPoints", () => {
  it("orders by finish time (not counter), drops unfinished runs", () => {
    // counter order ≠ finish order — counter is unique only per pipeline.
    const runs = [
      run({ counter: 3, started_at: "2026-01-01T00:00:00Z", finished_at: "2026-01-01T00:00:30Z" }),
      run({ counter: 1, started_at: "2026-01-01T00:00:00Z", finished_at: "2026-01-01T00:01:00Z" }),
      run({ counter: 2, started_at: "2026-01-01T00:00:00Z" }), // running — dropped
    ];
    const out = runDurationPoints(runs);
    expect(out.map((p) => p.label)).toEqual(["#3", "#1"]);
    expect(out.map((p) => p.durationSeconds)).toEqual([30, 60]);
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
        finished_at: new Date(Date.UTC(2026, 0, 1, 0, 0, 10 + i)).toISOString(),
      }),
    );
    const out = runDurationPoints(runs, 30);
    expect(out).toHaveLength(30);
    expect(out[0]!.label).toBe("#11");
    expect(out[29]!.label).toBe("#40");
  });
});

describe("durationSummary", () => {
  it("returns null below 2 positive points", () => {
    expect(durationSummary(pts())).toBeNull();
    expect(durationSummary(pts(10))).toBeNull();
    expect(durationSummary(pts(0, 0))).toBeNull(); // zero durations filtered out
  });

  it("computes median + fastest/slowest", () => {
    const s = durationSummary(pts(30, 10, 20))!;
    expect(s.median).toBe(20);
    expect(s.min).toBe(10);
    expect(s.max).toBe(30);
    expect(s.values).toEqual([30, 10, 20]); // preserves chronological order
  });

  it("flags a regression: recent half slower than prior", () => {
    const s = durationSummary(pts(60, 60, 120, 120))!;
    expect(s.deltaPct).toBe(100); // 60 → 120 median
    expect(s.slower).toBe(true);
  });

  it("flags an improvement: recent half faster", () => {
    const s = durationSummary(pts(120, 120, 60, 60))!;
    expect(s.deltaPct).toBe(-50); // 120 → 60 median
    expect(s.slower).toBe(false);
  });

  it("leaves delta null without enough history to split a window", () => {
    const s = durationSummary(pts(10, 20, 30))!;
    expect(s.deltaPct).toBeNull();
    expect(s.slower).toBe(false);
  });
});
