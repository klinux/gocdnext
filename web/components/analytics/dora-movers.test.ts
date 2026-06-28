import { describe, expect, it } from "vitest";

import type { DoraGroup } from "@/server/queries/analytics";

import { computeMovers } from "./dora-movers";

function g(name: string, over: Partial<DoraGroup>): DoraGroup {
  return {
    group: name,
    deploys_success: 10,
    deploys_total: 10,
    deploys_failed: 0,
    deploy_freq_per_day: 0.5,
    lead_time_p50_seconds: 3 * 86400,
    change_failure_rate: 0.18,
    mttr_p50_seconds: 5 * 3600,
    ...over,
  };
}

// payments: Medium → High (lead time cut 10d → 3d) = biggest improvement.
// card: High → Low (CFR 20% → 67%) = biggest regression.
// growth: 1 deploy = watch (fewest deploys).
const teams: DoraGroup[] = [
  g("payments", { deploys_total: 14, deploys_success: 12, deploys_failed: 2 }),
  g("card", {
    lead_time_p50_seconds: 120,
    change_failure_rate: 0.67,
    deploys_total: 21,
    deploys_success: 7,
    deploys_failed: 14,
    deploy_freq_per_day: 0.23,
  }),
  g("growth", {
    deploy_freq_per_day: 0.02,
    lead_time_p50_seconds: 5 * 86400,
    change_failure_rate: 0,
    mttr_p50_seconds: 0,
    deploys_total: 1,
    deploys_success: 1,
    deploys_failed: 0,
  }),
];

const prior: DoraGroup[] = [
  g("payments", { lead_time_p50_seconds: 10 * 86400 }), // was Medium (lead 10d)
  g("card", { change_failure_rate: 0.2, lead_time_p50_seconds: 120, deploy_freq_per_day: 0.23 }), // was High
  g("growth", {
    deploy_freq_per_day: 0.02,
    lead_time_p50_seconds: 5 * 86400,
    change_failure_rate: 0,
    mttr_p50_seconds: 0,
    deploys_total: 1,
  }),
];

describe("computeMovers", () => {
  it("picks improvement / regression / watch on distinct groups", () => {
    const movers = computeMovers(teams, prior, 30);
    const byKind = Object.fromEntries(movers.map((m) => [m.kind, m]));

    expect(byKind.up?.team).toBe("payments");
    expect(byKind.up?.text).toContain("improved from Medium to High");

    expect(byKind.down?.team).toBe("card");
    expect(byKind.down?.text).toContain("change failure rose to 67%");
    expect(byKind.down?.foot).toContain("+47pp");

    expect(byKind.watch?.team).toBe("growth");
    expect(byKind.watch?.text).toContain("shipped 1 successful deploy in 30d");

    // No group appears in two movers.
    const teamsPicked = movers.map((m) => m.team);
    expect(new Set(teamsPicked).size).toBe(teamsPicked.length);
  });

  it("emits no Watch when every group ships at a healthy cadence", () => {
    // Two healthy groups, one clearly improving — no stalled / dropping cadence.
    const healthy: DoraGroup[] = [
      g("payments", { deploy_freq_per_day: 0.5 }),
      g("identity", { deploy_freq_per_day: 0.7, lead_time_p50_seconds: 2 * 86400 }),
    ];
    const healthyPrior: DoraGroup[] = [
      g("payments", { deploy_freq_per_day: 0.5, lead_time_p50_seconds: 10 * 86400 }),
      g("identity", { deploy_freq_per_day: 0.7, lead_time_p50_seconds: 2 * 86400 }),
    ];
    const movers = computeMovers(healthy, healthyPrior, 30);
    expect(movers.some((m) => m.kind === "watch")).toBe(false);
  });

  it("returns nothing when there are no groups", () => {
    expect(computeMovers([], [], 30)).toEqual([]);
  });
});
