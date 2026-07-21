import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { TrafficBar } from "./traffic-bar";

describe("TrafficBar", () => {
  it("derives the segment widths and accessible label from canary_weight", () => {
    render(<TrafficBar canaryWeight={40} stableHash="aaaa1111bb" podHash="cccc2222dd" />);
    const bar = screen.getByRole("img", {
      name: /canary 40%, stable 60%/,
    });
    // Segment order is stable then canary; widths are inline percentages.
    const [stableSeg, canarySeg] = Array.from(bar.children) as HTMLElement[];
    expect(stableSeg?.style.width).toBe("60%");
    expect(canarySeg?.style.width).toBe("40%");
  });

  it("clamps an out-of-range weight to 100% canary", () => {
    render(<TrafficBar canaryWeight={150} stableHash="a" podHash="b" />);
    const bar = screen.getByRole("img", { name: /canary 100%, stable 0%/ });
    const [stableSeg, canarySeg] = Array.from(bar.children) as HTMLElement[];
    expect(stableSeg?.style.width).toBe("0%");
    expect(canarySeg?.style.width).toBe("100%");
  });

  it("labels the stable and canary revisions in the legend", () => {
    render(<TrafficBar canaryWeight={25} stableHash="stable9999x" podHash="canary8888y" />);
    expect(screen.getByText(/stable stable9999/)).toBeTruthy();
    expect(screen.getByText(/canary canary8888/)).toBeTruthy();
  });
});
