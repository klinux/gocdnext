import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DoraCompliance } from "./dora-compliance";
import type { ComplianceCoverageReport } from "@/server/queries/analytics";

function report(over: Partial<ComplianceCoverageReport> = {}): ComplianceCoverageReport {
  return { key: "team", groups: [], ...over };
}

describe("DoraCompliance", () => {
  it("shows an empty state when there are no groups", () => {
    render(<DoraCompliance report={report()} groupKey="team" />);
    expect(screen.getByText(/groups yet/i)).toBeTruthy();
  });

  it("renders per-framework coverage with percentage and counts", () => {
    render(
      <DoraCompliance
        report={report({
          groups: [
            {
              group: "payments",
              projects_total: 2,
              frameworks: [
                { framework: "ISO27001", covered: 1 },
                { framework: "SOC2", covered: 2 },
              ],
            },
          ],
        })}
        groupKey="team"
      />,
    );
    expect(screen.getByText("payments")).toBeTruthy();
    expect(screen.getByText("2 projects")).toBeTruthy();
    expect(screen.getByText("ISO27001")).toBeTruthy();
    expect(screen.getByText(/50% \(1\/2\)/)).toBeTruthy();
    expect(screen.getByText(/100% \(2\/2\)/)).toBeTruthy();
  });

  it("notes when a group has no frameworks bound", () => {
    render(
      <DoraCompliance
        report={report({
          groups: [{ group: "storefront", projects_total: 1, frameworks: [] }],
        })}
        groupKey="team"
      />,
    );
    expect(screen.getByText("1 project")).toBeTruthy();
    expect(screen.getByText(/no frameworks bound/i)).toBeTruthy();
  });
});
