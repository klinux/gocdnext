import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DoraBuildTimes, fmtBuild } from "./dora-build-times";
import type { BuildTimesReport } from "@/server/queries/analytics";

function report(over: Partial<BuildTimesReport> = {}): BuildTimesReport {
  return {
    key: "team",
    window_days: 30,
    overall: { builds: 0, p50_seconds: 0, p85_seconds: 0, p95_seconds: 0 },
    groups: [],
    ...over,
  };
}

describe("fmtBuild", () => {
  it("renders sub-minute in seconds, longer via the duration formatter, zero as dash", () => {
    expect(fmtBuild(0)).toBe("—");
    expect(fmtBuild(-5)).toBe("—");
    expect(fmtBuild(42)).toBe("42s");
    expect(fmtBuild(330)).toBe("5m 30s");
  });
});

describe("DoraBuildTimes", () => {
  it("shows an empty state when there are no build runs", () => {
    render(<DoraBuildTimes report={report()} groupKey="team" />);
    expect(screen.getByText(/no build runs in this window/i)).toBeTruthy();
    // and never pretends there is data
    expect(screen.queryByText(/p50 \(median\)/i)).toBeNull();
  });

  it("renders the three percentile tiles with formatted durations", () => {
    render(
      <DoraBuildTimes
        report={report({
          overall: { builds: 10, p50_seconds: 330, p85_seconds: 519, p95_seconds: 573 },
        })}
        groupKey="team"
      />,
    );
    expect(screen.getByText(/p50 \(median\)/i)).toBeTruthy();
    expect(screen.getByText("5m 30s")).toBeTruthy(); // p50 330s
    expect(screen.getByText("8m 39s")).toBeTruthy(); // p85 519s
    expect(screen.getByText("9m 33s")).toBeTruthy(); // p95 573s
    expect(screen.getByText(/10 builds over 30 days/i)).toBeTruthy();
  });

  it("renders a per-group build-time table scoped to the group key", () => {
    render(
      <DoraBuildTimes
        report={report({
          overall: { builds: 13, p50_seconds: 300, p85_seconds: 500, p95_seconds: 560 },
          groups: [
            { group: "payments", builds: 10, p50_seconds: 330, p85_seconds: 519, p95_seconds: 573 },
            { group: "storefront", builds: 3, p50_seconds: 200, p85_seconds: 260, p95_seconds: 290 },
          ],
        })}
        groupKey="team"
      />,
    );
    expect(screen.getByText(/build time by team/i)).toBeTruthy();
    expect(screen.getByText("payments")).toBeTruthy();
    expect(screen.getByText("storefront")).toBeTruthy();
    // storefront p50 200s → "3m 20s"
    expect(screen.getByText("3m 20s")).toBeTruthy();
  });
});
