// Maps a DoraOverview into the four hero-card descriptors (value, tier, delta,
// trend series, color) and the overall org tier. Pure — depends only on the
// query types + the dora lib, so it is unit-tested without React.

import type { DoraGroup, DoraOverview } from "@/server/queries/analytics";
import {
  type Delta,
  type Tier,
  TIER_COLOR,
  cfrTier,
  fmtDuration,
  fmtFreq,
  freqTier,
  leadTier,
  mttrTier,
  overallTier,
  pctDelta,
  ppDelta,
} from "@/lib/dora";

export type HeroMetric = {
  key: string;
  value: string;
  unit: string;
  tier: Tier;
  delta: Delta;
  vs: string;
  bench: string;
  // Daily trend (oldest→newest); may be flat when no daily breakdown exists.
  series: number[];
  color: string;
};

function trimNum(v: number): string {
  return v.toFixed(1).replace(/\.0$/, "");
}

export function heroMetrics(ov: DoraOverview): HeroMetric[] {
  const cur = ov.current;
  const prior = ov.prior;

  const freqSeries = ov.daily.map((d) => d.deploys_total);
  const leadSeries = ov.daily
    .map((d) => d.lead_time_p50_seconds)
    .filter((v) => v > 0);
  const cfrSeries = ov.daily.map((d) =>
    d.deploys_total > 0 ? d.deploys_failed / d.deploys_total : 0,
  );

  const freq: HeroMetric = {
    key: "Deploy frequency",
    value: trimNum(cur.deploy_freq_per_day),
    unit: "/dia",
    tier: freqTier(cur.deploy_freq_per_day),
    delta: pctDelta(cur.deploy_freq_per_day, prior.deploy_freq_per_day, false),
    vs: `${cur.deploys_success} deploys`,
    bench: "Elite: on-demand",
    series: freqSeries,
    color: TIER_COLOR[freqTier(cur.deploy_freq_per_day)],
  };

  const lead: HeroMetric = {
    key: "Lead time",
    value: fmtDuration(cur.lead_time_p50_seconds),
    unit: "",
    tier: leadTier(cur.lead_time_p50_seconds),
    delta: pctDelta(cur.lead_time_p50_seconds, prior.lead_time_p50_seconds, true),
    vs: "run → deploy p50",
    bench: "Elite: < 1 dia",
    series: leadSeries,
    color: TIER_COLOR[leadTier(cur.lead_time_p50_seconds)],
  };

  const cfr: HeroMetric = {
    key: "Change failure",
    value: String(Math.round(cur.change_failure_rate * 100)),
    unit: "%",
    tier: cfrTier(cur.change_failure_rate),
    delta: ppDelta(cur.change_failure_rate, prior.change_failure_rate),
    vs: `${cur.deploys_failed}/${cur.deploys_total} falharam`,
    bench: "Elite: 0–15%",
    series: cfrSeries,
    color: TIER_COLOR[cfrTier(cur.change_failure_rate)],
  };

  const mttr: HeroMetric = {
    key: "Time to restore",
    value: fmtDuration(cur.mttr_p50_seconds),
    unit: "",
    tier: mttrTier(cur.mttr_p50_seconds),
    delta: pctDelta(cur.mttr_p50_seconds, prior.mttr_p50_seconds, true),
    vs: "p50 restore",
    bench: "Elite: < 1 hora",
    // No daily MTTR breakdown — flat baseline from the window value.
    series: [cur.mttr_p50_seconds, cur.mttr_p50_seconds],
    color: TIER_COLOR[mttrTier(cur.mttr_p50_seconds)],
  };

  return [freq, lead, cfr, mttr];
}

// orgTier is the overall performer band from the four metric tiers.
export function orgTier(ov: DoraOverview): Tier {
  return overallTier(heroMetrics(ov).map((m) => m.tier));
}

// teamTier is a single group's performer band: the overall of its four metric
// tiers (a fast-but-unstable team — great freq/lead, awful CFR — lands Low,
// matching the handoff's "card" example).
export function teamTier(g: DoraGroup): Tier {
  return overallTier([
    freqTier(g.deploy_freq_per_day),
    leadTier(g.lead_time_p50_seconds),
    cfrTier(g.change_failure_rate),
    mttrTier(g.mttr_p50_seconds),
  ]);
}
