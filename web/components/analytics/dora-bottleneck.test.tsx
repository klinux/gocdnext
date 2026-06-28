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
    coding_p50_seconds: 3600, // 1h
    review_p50_seconds: 7200, // 2h
    release_wait_p50_seconds: 10800, // 3h — biggest
    deploy_p50_seconds: 1200, // 20m
    ...over,
  };
}

describe("DoraBottleneck", () => {
  it("renders the stages, total, and flags the biggest stage", () => {
    render(<DoraBottleneck bottleneck={b({})} />);
    expect(screen.getByText("Where lead time is lost")).toBeTruthy();
    expect(screen.getByText("6h 20m")).toBeTruthy(); // 1+2+3h+20m
    // The biggest stage (Release wait, 10800/22800 ≈ 47%) is flagged.
    expect(screen.getByText(/biggest stage — 47%/)).toBeTruthy();
    // Sample-transparency footer.
    expect(
      screen.getByText(/p50 across 5 PR-correlated deploys · 1 excluded/),
    ).toBeTruthy();
  });

  it("notes a smaller Review sample when fewer deploys have an approval", () => {
    render(<DoraBottleneck bottleneck={b({ review_sample: 2 })} />);
    expect(screen.getByText(/Review from 2 with an approval/)).toBeTruthy();
  });

  it("shows an info state when nothing correlates to a PR", () => {
    render(<DoraBottleneck bottleneck={b({ correlated: 0, excluded: 3 })} />);
    expect(screen.getByText(/No PR-correlated deploys in this window/)).toBeTruthy();
    expect(screen.getByText(/3 excluded/)).toBeTruthy();
  });
});
