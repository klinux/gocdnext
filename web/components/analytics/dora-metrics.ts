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
  // "—" when the metric has no sample (e.g. lead time with no successful
  // deploy). Tier is null in that case so the card shows no (misleading) band.
  value: string;
  unit: string;
  tier: Tier | null;
  delta: Delta;
  vs: string;
  bench: string;
  // Daily trend (oldest→newest); may be flat when no daily breakdown exists.
  series: number[];
  color: string;
};

const NEUTRAL = "var(--muted-foreground)";

function trimNum(v: number): string {
  return v.toFixed(1).replace(/\.0$/, "");
}

export function heroMetrics(ov: DoraOverview): HeroMetric[] {
  const cur = ov.current;
  const prior = ov.prior;

  // Deploy-frequency trend tracks *successful* deploys per day (matching the
  // card's success/day value) — not total, so a failure-heavy day doesn't lift
  // the line without a real delivery.
  const freqSeries = ov.daily.map((d) => d.deploys_success);
  const leadSeries = ov.daily
    .map((d) => d.lead_time_p50_seconds)
    .filter((v) => v > 0);
  const cfrSeries = ov.daily.map((d) =>
    d.deploys_total > 0 ? d.deploys_failed / d.deploys_total : 0,
  );

  // Sample guards: lead/CFR/MTTR with no underlying sample must not classify
  // as Elite (a 0-second median is "no data", not "instant").
  const hasLead = cur.deploys_success > 0 && cur.lead_time_p50_seconds > 0;
  const hasCfr = cur.deploys_total > 0;
  const hasMttr = cur.mttr_p50_seconds > 0;

  const freqTierV = freqTier(cur.deploy_freq_per_day);
  const freq: HeroMetric = {
    key: "Deploy frequency",
    value: trimNum(cur.deploy_freq_per_day),
    unit: "/dia",
    tier: freqTierV,
    delta: pctDelta(cur.deploy_freq_per_day, prior.deploy_freq_per_day, false),
    vs: `${cur.deploys_success} deploys`,
    bench: "Elite: on-demand",
    series: freqSeries,
    color: TIER_COLOR[freqTierV],
  };

  const leadTierV = hasLead ? leadTier(cur.lead_time_p50_seconds) : null;
  const lead: HeroMetric = {
    key: "Lead time",
    value: hasLead ? fmtDuration(cur.lead_time_p50_seconds) : "—",
    unit: "",
    tier: leadTierV,
    delta: hasLead
      ? pctDelta(cur.lead_time_p50_seconds, prior.lead_time_p50_seconds, true)
      : { text: "—", good: null },
    vs: hasLead ? "run → deploy p50" : "sem deploy concluído",
    bench: "Elite: < 1 dia",
    series: leadSeries,
    color: leadTierV ? TIER_COLOR[leadTierV] : NEUTRAL,
  };

  const cfrTierV = hasCfr ? cfrTier(cur.change_failure_rate) : null;
  const cfr: HeroMetric = {
    key: "Change failure",
    value: hasCfr ? String(Math.round(cur.change_failure_rate * 100)) : "—",
    unit: hasCfr ? "%" : "",
    tier: cfrTierV,
    delta: hasCfr
      ? ppDelta(cur.change_failure_rate, prior.change_failure_rate)
      : { text: "—", good: null },
    vs: hasCfr ? `${cur.deploys_failed}/${cur.deploys_total} falharam` : "sem deploys",
    bench: "Elite: 0–15%",
    series: cfrSeries,
    color: cfrTierV ? TIER_COLOR[cfrTierV] : NEUTRAL,
  };

  const mttrTierV = hasMttr ? mttrTier(cur.mttr_p50_seconds) : null;
  const mttr: HeroMetric = {
    key: "Time to restore",
    value: hasMttr ? fmtDuration(cur.mttr_p50_seconds) : "—",
    unit: "",
    tier: mttrTierV,
    delta: hasMttr
      ? pctDelta(cur.mttr_p50_seconds, prior.mttr_p50_seconds, true)
      : { text: "—", good: null },
    vs: hasMttr ? "p50 restore" : "sem restaurações",
    bench: "Elite: < 1 hora",
    // No daily MTTR breakdown — flat baseline from the window value.
    series: [cur.mttr_p50_seconds, cur.mttr_p50_seconds],
    color: mttrTierV ? TIER_COLOR[mttrTierV] : NEUTRAL,
  };

  return [freq, lead, cfr, mttr];
}

// orgTier is the overall performer band from the metric tiers that have a
// sample (null/no-data metrics are excluded so they can't inflate the verdict).
export function orgTier(ov: DoraOverview): Tier {
  const tiers = heroMetrics(ov)
    .map((m) => m.tier)
    .filter((t): t is Tier => t !== null);
  return overallTier(tiers);
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
