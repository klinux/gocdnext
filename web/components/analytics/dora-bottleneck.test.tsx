import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { LeadTimeBottleneck } from "@/server/queries/analytics";

import { DoraBottleneck } from "./dora-bottleneck";

function b(over: Partial<LeadTimeBottleneck>): LeadTimeBottleneck {
  return {
    correlated: 5,
    excluded: 1,
    coding_sample: 5,
    review_sample: 5,
    release_sample: 5,
    deploy_sample: 5,
    total_p50_seconds: 21600, // 6h — a TRUE median, distinct from the stage sum
    coding_p50_seconds: 3600, // 1h
    review_p50_seconds: 7200, // 2h
    release_wait_p50_seconds: 10800, // 3h — biggest
    deploy_p50_seconds: 1200, // 20m
    ...over,
  };
}

describe("DoraBottleneck", () => {
  it("shows the true end-to-end p50 (not the stage sum) and flags the biggest stage", () => {
    render(<DoraBottleneck bottleneck={b({})} />);
    expect(screen.getByText("Where lead time is lost")).toBeTruthy();
    // Header is the true p50 (6h), NOT the 6h20m sum of the stage p50s.
    expect(screen.getByText("6h")).toBeTruthy();
    expect(screen.getByText("lead time · p50")).toBeTruthy();
    // Biggest stage (Release wait, 10800/22800 ≈ 47% of the decomposed time).
    expect(screen.getByText(/biggest stage — 47%/)).toBeTruthy();
    expect(
      screen.getByText(/Stage medians across 5 PR-correlated deploys · 1 excluded/),
    ).toBeTruthy();
  });

  it("shows a per-stage sample (n) when a stage has fewer deploys", () => {
    render(<DoraBottleneck bottleneck={b({ review_sample: 2 })} />);
    expect(screen.getByText("n 2")).toBeTruthy();
  });

  it("shows an info state when nothing correlates to a PR", () => {
    render(<DoraBottleneck bottleneck={b({ correlated: 0, excluded: 3 })} />);
    expect(screen.getByText(/No PR-correlated deploys in this window/)).toBeTruthy();
    expect(screen.getByText(/3 excluded/)).toBeTruthy();
  });

  it("distinguishes correlated-but-no-timings from no correlation", () => {
    render(
      <DoraBottleneck
        bottleneck={b({
          total_p50_seconds: 0,
          coding_p50_seconds: 0,
          review_p50_seconds: 0,
          release_wait_p50_seconds: 0,
          deploy_p50_seconds: 0,
        })}
      />,
    );
    expect(screen.getByText(/no stage timings yet/)).toBeTruthy();
  });
});
