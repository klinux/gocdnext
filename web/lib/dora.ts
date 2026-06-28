// Pure DORA helpers: tier classification against the standard benchmark
// thresholds, value formatters, and window-over-window deltas with semantic
// goodness (for lead time / CFR / MTTR a *decrease* is the good direction).
// No I/O, no React — unit-tested in dora.test.ts.

export type Tier = "elite" | "high" | "medium" | "low";

export const TIER_RANK: Record<Tier, number> = {
  elite: 4,
  high: 3,
  medium: 2,
  low: 1,
};

export const TIER_LABEL: Record<Tier, string> = {
  elite: "Elite",
  high: "High",
  medium: "Medium",
  low: "Low",
};

// Tier → hex, matching the handoff (Elite=teal, High=green, Medium=amber,
// Low=red) and the existing duration-sparkline palette.
export const TIER_COLOR: Record<Tier, string> = {
  elite: "#45c8d4",
  high: "#3fb950",
  medium: "#d9a429",
  low: "#f85149",
};

const DAY = 86_400;
const WEEK = 7 * DAY;
const MONTH = 30 * DAY;
const HOUR = 3_600;

// Deployment frequency (successes/day): on-demand → daily → weekly → monthly.
export function freqTier(perDay: number): Tier {
  if (perDay >= 1) return "elite";
  if (perDay >= 1 / 7) return "high";
  if (perDay >= 1 / 30) return "medium";
  return "low";
}

// Lead time for changes (seconds): < 1 day / < 1 week / < 1 month / more.
export function leadTier(seconds: number): Tier {
  if (seconds < DAY) return "elite";
  if (seconds < WEEK) return "high";
  if (seconds < MONTH) return "medium";
  return "low";
}

// Change failure rate (0..1): ≤15% / ≤30% / ≤45% / more.
export function cfrTier(rate: number): Tier {
  if (rate <= 0.15) return "elite";
  if (rate <= 0.3) return "high";
  if (rate <= 0.45) return "medium";
  return "low";
}

// Time to restore (seconds): < 1 hour / < 1 day / < 1 week / more.
export function mttrTier(seconds: number): Tier {
  if (seconds < HOUR) return "elite";
  if (seconds < DAY) return "high";
  if (seconds < WEEK) return "medium";
  return "low";
}

// Overall tier = the weakest link. A unit is only as good as its worst DORA
// metric, so a team that ships fast but reverts 67% of the time lands Low, not
// High — the verdict a manager actually needs. (DORA's own banding penalises a
// single catastrophic dimension the same way.)
export function overallTier(tiers: Tier[]): Tier {
  if (tiers.length === 0) return "low";
  const worst = Math.min(...tiers.map((t) => TIER_RANK[t]));
  return (
    (Object.keys(TIER_RANK) as Tier[]).find((t) => TIER_RANK[t] === worst) ??
    "low"
  );
}

// fmtDuration renders seconds the way the handoff does: "5 dias", "18h",
// "3h 12m", "48m", "2m". Days only kick in at ≥ 2 days (so a 28h restore reads
// "28h", not "1 dia 4h"). Zero / negative → "—" (no data).
export function fmtDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "—";
  const days = Math.floor(seconds / DAY);
  if (days >= 2) return `${days} dias`;
  const hours = Math.floor(seconds / HOUR);
  const mins = Math.round((seconds % HOUR) / 60);
  if (hours >= 1) return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`;
  return `${Math.max(1, mins)}m`;
}

// fmtFreq renders a per-day rate in the requested cadence: "1.9/dia" for the
// hero, "3.3/sem" for the leaderboard. One decimal, trimmed.
export function fmtFreq(perDay: number, unit: "dia" | "sem" = "dia"): string {
  const v = unit === "sem" ? perDay * 7 : perDay;
  const s = v.toFixed(1).replace(/\.0$/, "");
  return `${s}/${unit}`;
}

// fmtPct renders a 0..1 rate as a whole-percent string ("24%").
export function fmtPct(rate: number): string {
  return `${Math.round(rate * 100)}%`;
}

export type Delta = {
  // Display string, e.g. "+18%", "−24%", "+6pp", or "—" when flat / undefined.
  text: string;
  // true = improvement, false = regression, null = flat or not computable.
  good: boolean | null;
};

const FLAT: Delta = { text: "—", good: null };

// pctDelta: relative change cur vs prior, signed, as a percent. `lowerIsBetter`
// flips the goodness (lead time / MTTR improve by falling). Prior ≤ 0 → flat
// (no baseline to compare against).
export function pctDelta(
  cur: number,
  prior: number,
  lowerIsBetter: boolean,
): Delta {
  if (!(prior > 0) || !Number.isFinite(cur)) return FLAT;
  const change = (cur - prior) / prior;
  if (Math.abs(change) < 0.005) return FLAT;
  const pct = Math.round(Math.abs(change) * 100);
  const rising = change > 0;
  const sign = rising ? "+" : "−";
  const good = lowerIsBetter ? !rising : rising;
  return { text: `${sign}${pct}%`, good };
}

// ppDelta: percentage-point change for rates already in 0..1 (CFR). Always
// "lower is better". Flat when within half a point.
export function ppDelta(curRate: number, priorRate: number): Delta {
  const pp = Math.round((curRate - priorRate) * 100);
  if (pp === 0) return FLAT;
  const rising = pp > 0;
  return {
    text: `${rising ? "+" : "−"}${Math.abs(pp)}pp`,
    good: !rising,
  };
}
