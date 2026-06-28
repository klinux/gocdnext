import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { DoraGroup } from "@/server/queries/analytics";

import { DoraLeaderboard } from "./dora-leaderboard.client";

function g(name: string, over: Partial<DoraGroup>): DoraGroup {
  return {
    group: name,
    deploys_success: 10,
    deploys_total: 10,
    deploys_failed: 0,
    deploy_freq_per_day: 1,
    lead_time_p50_seconds: 3600,
    change_failure_rate: 0,
    mttr_p50_seconds: 0,
    ...over,
  };
}

const teams: DoraGroup[] = [
  g("platform", { lead_time_p50_seconds: 120 }), // 2m — fastest
  g("data-eng", { lead_time_p50_seconds: 2 * 86400 }), // 2 days — slowest
  g("payments", { lead_time_p50_seconds: 4 * 3600 + 10 * 60 }), // 4h 10m
];

function rowNames(): string[] {
  // Skip the header row; read the first cell (team name) of each body row.
  return screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).getAllByRole("cell")[0]!.textContent!.replace("team:", "").trim());
}

describe("DoraLeaderboard", () => {
  it("renders one row per team with the team prefix", () => {
    render(<DoraLeaderboard teams={teams} groupKey="team" />);
    expect(screen.getAllByText("team:")).toHaveLength(3);
    expect(rowNames().sort()).toEqual(["data-eng", "payments", "platform"]);
  });

  it("sorts by lead time ascending then toggles descending on header click", async () => {
    const user = userEvent.setup();
    render(<DoraLeaderboard teams={teams} groupKey="team" />);

    const leadHeader = screen.getByText("Lead time");
    await user.click(leadHeader); // first click on a numeric col → descending
    expect(rowNames()[0]).toBe("data-eng"); // slowest first (desc)

    await user.click(leadHeader); // toggle → ascending
    expect(rowNames()[0]).toBe("platform"); // fastest first (asc)
  });
});
