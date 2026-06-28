import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { DoraDashboard } from "./dora-dashboard.client";
import type { DoraGroup } from "@/server/queries/analytics";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));

const groups: DoraGroup[] = [
  {
    group: "payments",
    deploys_success: 9,
    deploys_total: 10,
    deploys_failed: 1,
    deploy_freq_per_day: 0.3,
    lead_time_p50_seconds: 3600,
    change_failure_rate: 0.1,
    mttr_p50_seconds: 7200,
  },
];

describe("DoraDashboard", () => {
  it("renders a card per group with the four DORA metrics", () => {
    render(
      <DoraDashboard keys={["team"]} activeKey="team" windowDays={30} groups={groups} />,
    );
    expect(screen.getByText("team:payments")).toBeTruthy();
    expect(screen.getByText("9/10 deploys")).toBeTruthy();
    // change failure rate 0.1 → 10%
    expect(screen.getByText("10%")).toBeTruthy();
    // lead time 3600s → 1h ; mttr 7200s → 2h
    expect(screen.getByText("1h")).toBeTruthy();
    expect(screen.getByText("2h")).toBeTruthy();
  });

  it("shows an empty state when no group has deploys in the window", () => {
    render(<DoraDashboard keys={["team"]} activeKey="team" windowDays={30} groups={[]} />);
    expect(screen.getByText(/No deployments in this window/i)).toBeTruthy();
  });
});
