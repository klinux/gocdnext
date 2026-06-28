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
      ? `change failure rose to ${fmtPct(r.c.change_failure_rate)} — ${r.c.deploys_failed}/${r.c.deploys_total} change failures.`
      : `dropped from ${TIER_LABEL[r.tierPrior!]} to ${TIER_LABEL[r.tierCur]}.`,
    foot:
      r.cfrDeltaPp > 0
        ? `CFR +${r.cfrDeltaPp}pp vs. prior window.`
        : `Now ${TIER_LABEL[r.tierCur]} tier.`,
  };
}

// Cadence thresholds: "stalled" is fewer than ~1 successful deploy every two
// weeks; a "hard drop" is a ≥40% fall in frequency vs. the prior window while
// already below weekly. Only a group meeting one of these is worth flagging —
// so a healthy org shows no Watch card (no false alarm).
const STALLED_PER_DAY = 1 / 14;
const WEEKLY_PER_DAY = 1 / 7;

function watch(recs: Rec[], taken: Set<string>, windowDays: number): Mover | null {
  const scored = recs
    .filter((r) => !taken.has(r.c.group))
    .map((r) => {
      const priorFreq = r.p?.deploy_freq_per_day ?? 0;
      const freqDrop =
        priorFreq > 0 ? (priorFreq - r.c.deploy_freq_per_day) / priorFreq : 0;
      const stalled = r.c.deploy_freq_per_day < STALLED_PER_DAY;
      const droppedHard = freqDrop >= 0.4 && r.c.deploy_freq_per_day < WEEKLY_PER_DAY;
      return { r, freqDrop, qualifies: stalled || droppedHard };
    })
    .filter((x) => x.qualifies)
    .sort((a, b) => a.r.c.deploy_freq_per_day - b.r.c.deploy_freq_per_day);

  const top = scored[0];
  if (!top) return null;
  const c = top.r.c;
  return {
    kind: "watch",
    team: c.group,
    text: `shipped ${c.deploys_success} successful deploy${c.deploys_success === 1 ? "" : "s"} in ${windowDays}d — cadence at risk.`,
    foot:
      top.freqDrop >= 0.4
        ? `Deploy frequency −${pct(top.freqDrop)}% vs. prior window.`
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
