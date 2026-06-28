import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DurationSparkline } from "./duration-sparkline";
import { DurationTrendPill } from "./duration-trend-pill.client";
import type { DurationPoint } from "./duration-trend";

function pts(...durs: number[]): DurationPoint[] {
  return durs.map((d, i) => ({ label: `#${i + 1}`, durationSeconds: d, status: "success" }));
}

describe("DurationSparkline", () => {
  it("renders nothing below 2 positive points", () => {
    const { container } = render(<DurationSparkline values={[10]} />);
    expect(container.querySelector("svg")).toBeNull();
  });

  it("renders a line path + median reference with enough data", () => {
    const { container } = render(<DurationSparkline values={[10, 20, 30]} />);
    expect(container.querySelector("path")).not.toBeNull();
    // median ref uses currentColor so it tracks the theme (not hardcoded white)
    expect(container.querySelector("line")?.getAttribute("stroke")).toBe("currentColor");
  });

  it("stays all-teal when the series never regresses (no amber/red sliver)", () => {
    const { container } = render(<DurationSparkline values={[100, 100, 100, 100]} />);
    const html = container.innerHTML;
    expect(html).toContain("var(--teal)");
    expect(html).not.toContain("var(--amber)");
    expect(html).not.toContain("var(--red)");
  });

  it("adds amber/red stops once the series regresses past median", () => {
    const { container } = render(<DurationSparkline values={[50, 50, 200, 200]} />);
    expect(container.innerHTML).toContain("var(--red)");
  });
});

describe("DurationTrendPill", () => {
  it("renders nothing below 2 finished runs", () => {
    const { container } = render(<DurationTrendPill points={pts(10)} />);
    expect(container.firstChild).toBeNull();
  });

  it("shows median + regression delta, collapsed by default", () => {
    render(<DurationTrendPill points={pts(60, 60, 120, 120)} />);
    const trigger = screen.getByRole("button");
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(screen.getByText("1m 30s")).toBeTruthy(); // median
    expect(screen.getByText(/100%/)).toBeTruthy(); // delta badge
    // histogram is not mounted until opened
    expect(screen.queryByText(/fastest/)).toBeNull();
  });

  it("opens the histogram on click and closes on Escape", () => {
    render(<DurationTrendPill points={pts(60, 60, 120, 120)} note="across all pipelines" />);
    const trigger = screen.getByRole("button");

    fireEvent.click(trigger);
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText("across all pipelines")).toBeTruthy();
    expect(screen.getByText(/fastest 1m 0s/)).toBeTruthy();
    expect(screen.getByText(/slowest 2m 0s/)).toBeTruthy();

    fireEvent.keyDown(document, { key: "Escape" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText(/fastest/)).toBeNull();
  });
});
