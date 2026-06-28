// Computes the "Highlights" movers from per-group current vs prior-window DORA
// rollups — biggest improvement (up), biggest regression (down), and a watch
// (stalled cadence). Pure + data-derived (no editorial drivers), so it is
// unit-tested without React.

import {
  TIER_LABEL,
  TIER_RANK,
  type Tier,
  fmtDuration,
  fmtPct,
} from "@/lib/dora";
import type { DoraGroup } from "@/server/queries/analytics";

import { teamTier } from "./dora-metrics";

export type Mover = {
  kind: "up" | "down" | "watch";
  team: string;
  // Sentence fragment after the accented team name (e.g. "improved …").
  text: string;
  // Secondary data-derived caption.
  foot: string;
};

type Rec = {
  c: DoraGroup;
  p: DoraGroup | undefined;
  tierCur: Tier;
  tierPrior: Tier | null;
  tierDelta: number;
  leadImprove: number; // 0..1 relative lead-time reduction
  cfrDeltaPp: number; // percentage-point CFR change (positive = worse)
};

function pct(frac: number): number {
  return Math.round(frac * 100);
}

function build(teams: DoraGroup[], prior: DoraGroup[]): Rec[] {
  const priorBy = new Map(prior.map((g) => [g.group, g]));
  return teams.map((c) => {
    const p = priorBy.get(c.group);
    const tierCur = teamTier(c);
    const tierPrior = p ? teamTier(p) : null;
    const tierDelta = tierPrior ? TIER_RANK[tierCur] - TIER_RANK[tierPrior] : 0;
    const leadImprove =
      p && p.lead_time_p50_seconds > 0 && c.lead_time_p50_seconds > 0
        ? (p.lead_time_p50_seconds - c.lead_time_p50_seconds) /
          p.lead_time_p50_seconds
        : 0;
    const cfrDeltaPp = pct(
      c.change_failure_rate - (p?.change_failure_rate ?? c.change_failure_rate),
    );
    return { c, p, tierCur, tierPrior, tierDelta, leadImprove, cfrDeltaPp };
  });
}

function improvement(recs: Rec[]): Mover | null {
  const cand = recs
    .filter((r) => r.tierDelta > 0 || r.leadImprove >= 0.15)
    .sort((a, b) => b.tierDelta - a.tierDelta || b.leadImprove - a.leadImprove);
  const r = cand[0];
  if (!r) return null;
  const tierStory = r.tierDelta > 0 && r.tierPrior;
  return {
    kind: "up",
    team: r.c.group,
    text: tierStory
      ? `improved from ${TIER_LABEL[r.tierPrior!]} to ${TIER_LABEL[r.tierCur]}.`
      : `cut lead time from ${fmtDuration(r.p!.lead_time_p50_seconds)} to ${fmtDuration(r.c.lead_time_p50_seconds)}.`,
    foot:
      r.leadImprove > 0
        ? `Lead time −${pct(r.leadImprove)}% vs. prior window.`
        : `Now ${TIER_LABEL[r.tierCur]} tier.`,
  };
}

function regression(recs: Rec[], taken: Set<string>): Mover | null {
  const cand = recs
    .filter((r) => !taken.has(r.c.group))
    .filter((r) => r.tierDelta < 0 || r.cfrDeltaPp >= 10)
    .sort((a, b) => a.tierDelta - b.tierDelta || b.cfrDeltaPp - a.cfrDeltaPp);
  const r = cand[0];
  if (!r) return null;
  const cfrStory = r.cfrDeltaPp >= 10 || r.c.change_failure_rate > 0.45;
  return {
    kind: "down",
    team: r.c.group,
    text: cfrStory
      ? `change failure rose to ${fmtPct(r.c.change_failure_rate)} — ${r.c.deploys_failed}/${r.c.deploys_total} reverted.`
      : `dropped from ${TIER_LABEL[r.tierPrior!]} to ${TIER_LABEL[r.tierCur]}.`,
    foot:
      r.cfrDeltaPp > 0
        ? `CFR +${r.cfrDeltaPp}pp vs. prior window.`
        : `Now ${TIER_LABEL[r.tierCur]} tier.`,
  };
}

function watch(recs: Rec[], taken: Set<string>, windowDays: number): Mover | null {
  // Stalled cadence: the remaining group shipping the fewest deploys.
  const cand = recs
    .filter((r) => !taken.has(r.c.group))
    .sort((a, b) => a.c.deploys_total - b.c.deploys_total);
  const r = cand[0];
  if (!r) return null;
  return {
    kind: "watch",
    team: r.c.group,
    text: `only shipped ${r.c.deploys_total} deploy${r.c.deploys_total === 1 ? "" : "s"} in ${windowDays}d — cadence at risk.`,
    foot:
      r.c.deploys_failed > 0
        ? `Change failure ${fmtPct(r.c.change_failure_rate)} on a thin sample.`
        : `Large-batch risk as changes accumulate.`,
  };
}

// computeMovers returns up to three movers (improvement / regression / watch),
// each on a distinct group; absent when nothing qualifies.
export function computeMovers(
  teams: DoraGroup[],
  prior: DoraGroup[],
  windowDays: number,
): Mover[] {
  if (teams.length === 0) return [];
  const recs = build(teams, prior);
  const taken = new Set<string>();

  const up = improvement(recs);
  if (up) taken.add(up.team);
  const down = regression(recs, taken);
  if (down) taken.add(down.team);
  const eye = watch(recs, taken, windowDays);

  return [up, down, eye].filter((m): m is Mover => m !== null);
}
