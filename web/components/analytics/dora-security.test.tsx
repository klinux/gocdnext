import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { DoraSecurity } from "./dora-security";
import type { SecurityRollupGroup, SecurityRollupReport } from "@/server/queries/analytics";

function group(over: Partial<SecurityRollupGroup> = {}): SecurityRollupGroup {
  return {
    group: "payments",
    has_scans: true,
    critical: 0,
    high: 0,
    medium: 0,
    low: 0,
    total_open: 0,
    accepted: 0,
    ...over,
  };
}

function report(groups: SecurityRollupGroup[]): SecurityRollupReport {
  return { key: "team", groups, org_critical: 0, org_high: 0, org_total_open: 0, org_accepted: 0 };
}

describe("DoraSecurity", () => {
  it("shows the severity breakdown for a group with findings", () => {
    render(
      <DoraSecurity
        report={report([group({ critical: 2, high: 3, total_open: 5 })])}
        groupKey="team"
      />,
    );
    expect(screen.getByText("2 Critical")).toBeTruthy();
    expect(screen.getByText("3 High")).toBeTruthy();
  });

  it("shows a scanned-clean group as clean, not dropped", () => {
    render(<DoraSecurity report={report([group({ has_scans: true })])} groupKey="team" />);
    expect(screen.getByText("payments")).toBeTruthy();
    expect(screen.getByText("Clean — no open findings.")).toBeTruthy();
  });

  it("flags a never-scanned group", () => {
    render(<DoraSecurity report={report([group({ has_scans: false })])} groupKey="team" />);
    expect(screen.getByText("no scans yet")).toBeTruthy();
  });

  it("shows accepted separately from open", () => {
    render(
      <DoraSecurity report={report([group({ accepted: 2 })])} groupKey="team" />,
    );
    expect(screen.getByText("2 accepted")).toBeTruthy();
  });

  it("renders an empty state when there are no groups", () => {
    render(<DoraSecurity report={report([])} groupKey="team" />);
    expect(screen.getByText(/No/)).toBeTruthy();
  });
});
