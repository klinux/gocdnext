import { describe, expect, it } from "vitest";

import type { DoraGroup, DoraOverview } from "@/server/queries/analytics";

import { heroMetrics, orgTier, teamTier } from "./dora-metrics";

function group(over: Partial<DoraGroup>): DoraGroup {
  return {
    group: "payments",
    deploys_success: 10,
    deploys_total: 12,
    deploys_failed: 2,
    deploy_freq_per_day: 1.5,
    lead_time_p50_seconds: 3600,
    change_failure_rate: 0.16,
    mttr_p50_seconds: 1800,
    ...over,
  };
}

function overview(over: Partial<DoraOverview>): DoraOverview {
  const cur = {
    deploys_success: 30,
    deploys_total: 40,
    deploys_failed: 10,
    deploy_freq_per_day: 1.9,
    lead_time_p50_seconds: 3 * 3600 + 12 * 60,
    change_failure_rate: 0.24,
    mttr_p50_seconds: 2 * 3600 + 10 * 60,
  };
  return {
    key: "team",
    window_days: 30,
    current: cur,
    prior: {
      ...cur,
      deploy_freq_per_day: 1.6,
      lead_time_p50_seconds: 4 * 3600,
      change_failure_rate: 0.18,
    },
    daily: [
      { day: "2026-06-01", deploys_total: 2, deploys_failed: 0, lead_time_p50_seconds: 600 },
      { day: "2026-06-02", deploys_total: 3, deploys_failed: 1, lead_time_p50_seconds: 700 },
    ],
    teams: [],
    ...over,
  };
}

describe("heroMetrics", () => {
  it("maps the four DORA metrics with value, tier and delta", () => {
    const m = heroMetrics(overview({}));
    expect(m).toHaveLength(4);

    const freq = m[0]!;
    expect(freq.key).toBe("Deploy frequency");
    expect(freq.value).toBe("1.9");
    expect(freq.unit).toBe("/dia");
    expect(freq.delta.good).toBe(true); // 1.9 vs 1.6 up = good

    const lead = m[1]!;
    expect(lead.value).toBe("3h 12m");
    expect(lead.delta.good).toBe(true); // 3h12 vs 4h down = good

    const cfr = m[2]!;
    expect(cfr.value).toBe("24");
    expect(cfr.unit).toBe("%");
    expect(cfr.delta.text).toBe("+6pp");
    expect(cfr.delta.good).toBe(false); // CFR up = bad
  });

  it("gives the MTTR card a flat fallback series (no daily breakdown)", () => {
    const m = heroMetrics(overview({}));
    const mttr = m[3]!;
    expect(mttr.key).toBe("Time to restore");
    expect(mttr.series).toHaveLength(2);
    expect(mttr.series[0]).toBe(mttr.series[1]);
  });
});

describe("tiers", () => {
  it("derives an org tier from the four metrics", () => {
    expect(orgTier(overview({}))).toMatch(/elite|high|medium|low/);
  });

  it("lands a fast-but-unstable team in Low (great freq/lead, awful CFR)", () => {
    // card: 2m lead, frequent, but 67% CFR.
    expect(
      teamTier(
        group({
          deploy_freq_per_day: 3,
          lead_time_p50_seconds: 120,
          change_failure_rate: 0.67,
          mttr_p50_seconds: 5 * 3600,
        }),
      ),
    ).toBe("low");
  });

  it("lands an all-around strong team in Elite", () => {
    expect(
      teamTier(
        group({
          deploy_freq_per_day: 2,
          lead_time_p50_seconds: 120,
          change_failure_rate: 0,
          mttr_p50_seconds: 600,
        }),
      ),
    ).toBe("elite");
  });
});
