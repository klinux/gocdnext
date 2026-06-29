import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DoraReliability } from "./dora-reliability";
import type { ReliabilityReport, ThroughputGroup } from "@/server/queries/analytics";

function group(over: Partial<ThroughputGroup> = {}): ThroughputGroup {
  return {
    group: "payments",
    runs_success: 15,
    runs_failed: 4,
    runs_total: 19,
    runs_per_day: 0.63,
    success_rate: 0.7895,
    queue_wait_p50_seconds: 30,
    duration_p50_seconds: 120,
    ...over,
  };
}

function report(over: Partial<ReliabilityReport> = {}): ReliabilityReport {
  return { key: "team", window_days: 30, groups: [], hotspots: [], ...over };
}

describe("DoraReliability", () => {
  it("shows an empty state when there are no groups", () => {
    render(<DoraReliability report={report()} groupKey="team" />);
    expect(screen.getByText(/no finished runs/i)).toBeTruthy();
  });

  it("renders throughput rows with the rounded success rate", () => {
    render(<DoraReliability report={report({ groups: [group()] })} groupKey="team" />);
    expect(screen.getByText("payments")).toBeTruthy();
    expect(screen.getByText("79%")).toBeTruthy(); // 0.7895 → 79%
  });

  it("lists hotspots with failure rate and links the project", () => {
    render(
      <DoraReliability
        report={report({
          groups: [group({ group: "x" })],
          hotspots: [
            {
              project_slug: "shop",
              project: "shop",
              pipeline: "web",
              runs_total: 8,
              runs_failed: 5,
              failure_rate: 0.625,
            },
          ],
        })}
        groupKey="team"
      />,
    );
    expect(screen.getByText("63%")).toBeTruthy(); // 0.625 → 63%
    expect(screen.getByText(/5\/8 failed/)).toBeTruthy();
    const link = screen.getByRole("link", { name: /shop/ });
    expect(link.getAttribute("href")).toBe("/projects/shop");
  });

  it("shows a clean-slate message when there are groups but no hotspots", () => {
    render(
      <DoraReliability
        report={report({ groups: [group({ success_rate: 1, runs_failed: 0 })] })}
        groupKey="team"
      />,
    );
    expect(screen.getByText(/clean slate/i)).toBeTruthy();
  });
});
