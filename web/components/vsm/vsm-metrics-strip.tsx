import { cn } from "@/lib/utils";
import { formatDurationSeconds } from "@/lib/format";
import type { VSMNode } from "@/types/api";

import type { VSMEdge } from "@/types/api";

type Props = {
  nodes: VSMNode[];
  edges: VSMEdge[];
  windowDays: number;
};

// VSMMetricsStrip renders the project-level rollup at the bottom of
// the canvas as a DORA-like panel: label on top, big value in the
// middle, contextual hint below. Weighted-by-runs averages — noisy
// low-volume pipelines don't skew a chatty project. True rolled
// %C/A (multiplicative along the critical path) and commit→prod
// lead time are flagged in the roadmap.
export function VSMMetricsStrip({ nodes, edges, windowDays }: Props) {
  const rollup = computeRollup(nodes);
  const rolled = computeRolledCA(nodes, edges);
  if (rollup.runsConsidered === 0) {
    return (
      <p className="border-t border-border bg-muted/20 px-6 py-3 text-xs italic text-muted-foreground">
        Not enough terminal runs in the last {windowDays}d to roll up metrics.
      </p>
    );
  }
  const ratePct = Math.round(rollup.avgSuccessRate * 100);
  return (
    <div className="flex flex-wrap items-stretch gap-x-10 gap-y-4 border-t border-border bg-muted/20 px-6 py-5">
      <Cell
        label="Lead P50 avg"
        value={formatDurationSeconds(rollup.avgLeadTimeSec)}
        hint="commit → terminal"
      />
      <Cell
        label="Process P50 avg"
        value={formatDurationSeconds(rollup.avgProcessTimeSec)}
        hint="actual busy time"
      />
      <Cell
        label="Success avg"
        value={`${ratePct}%`}
        hint={`over ${windowDays}d`}
        valueTone={
          ratePct >= 90
            ? "emerald"
            : ratePct >= 70
              ? "amber"
              : "red"
        }
      />
      <Cell
        label="Pipelines"
        value={`${rollup.pipelinesWithMetrics} / ${nodes.length}`}
        hint="with metrics"
      />
      {rolled != null ? (
        <Cell
          label="Rolled %C/A"
          value={`${Math.round(rolled.value * 100)}%`}
          hint={
            rolled.chainLength > 1
              ? `along ${rolled.chainLength}-pipeline path · meta ≥ 90%`
              : "meta ≥ 90%"
          }
          valueTone={
            rolled.value >= 0.9
              ? "emerald"
              : rolled.value >= 0.7
                ? "amber"
                : "red"
          }
        />
      ) : null}
      <span className="ml-auto self-end pb-1 text-[10px] tabular-nums text-muted-foreground">
        {rollup.runsConsidered} runs · {windowDays}d
      </span>
    </div>
  );
}

type CellProps = {
  label: string;
  value: string;
  hint?: string;
  valueTone?: "emerald" | "amber" | "red";
};

function Cell({ label, value, hint, valueTone }: CellProps) {
  return (
    <div className="flex min-w-[120px] flex-col gap-0.5">
      <span className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
        {label}
      </span>
      <span
        className={cn(
          "font-mono text-2xl font-semibold tabular-nums leading-none text-foreground",
          valueTone === "emerald" && "text-emerald-500",
          valueTone === "amber" && "text-amber-500",
          valueTone === "red" && "text-red-500",
        )}
      >
        {value}
      </span>
      {hint ? (
        <span className="text-[10px] text-muted-foreground">{hint}</span>
      ) : null}
    </div>
  );
}

function computeRollup(nodes: VSMNode[]): {
  avgLeadTimeSec: number;
  avgProcessTimeSec: number;
  avgSuccessRate: number;
  runsConsidered: number;
  pipelinesWithMetrics: number;
} {
  let leadWeighted = 0;
  let processWeighted = 0;
  let passWeighted = 0;
  let totalRuns = 0;
  let pipelinesWithMetrics = 0;
  for (const n of nodes) {
    const m = n.metrics;
    if (!m || m.runs_considered === 0) continue;
    pipelinesWithMetrics++;
    leadWeighted += m.lead_time_p50_seconds * m.runs_considered;
    processWeighted += m.process_time_p50_seconds * m.runs_considered;
    passWeighted += m.success_rate * m.runs_considered;
    totalRuns += m.runs_considered;
  }
  if (totalRuns === 0) {
    return {
      avgLeadTimeSec: 0,
      avgProcessTimeSec: 0,
      avgSuccessRate: 0,
      runsConsidered: 0,
      pipelinesWithMetrics: 0,
    };
  }
  return {
    avgLeadTimeSec: leadWeighted / totalRuns,
    avgProcessTimeSec: processWeighted / totalRuns,
    avgSuccessRate: passWeighted / totalRuns,
    runsConsidered: totalRuns,
    pipelinesWithMetrics,
  };
}

// computeRolledCA returns the end-to-end success probability along
// the longest dependency chain in the VSM (critical path). For
// projects with no edges it falls back to the product across all
// pipelines with metrics — interpreted as "the chance every
// pipeline holds on a single commit". Returns null when the VSM
// doesn't have enough data (no terminal runs at all).
function computeRolledCA(
  nodes: VSMNode[],
  edges: VSMEdge[],
): { value: number; chainLength: number } | null {
  const byName = new Map(nodes.map((n) => [n.name, n]));
  const parents = new Map<string, string[]>();
  for (const e of edges) {
    if (!byName.has(e.from_pipeline) || !byName.has(e.to_pipeline)) continue;
    const list = parents.get(e.to_pipeline) ?? [];
    list.push(e.from_pipeline);
    parents.set(e.to_pipeline, list);
  }

  // Longest-path DP: for each node compute (value, length) of the
  // best chain ending at that node — "best" means product of
  // success rates over the longest path, so a node with no data
  // stops the chain rather than silently multiplying by 1.
  const best = new Map<string, { value: number; length: number } | null>();
  const visit = (name: string, seen: Set<string>): { value: number; length: number } | null => {
    const cached = best.get(name);
    if (cached !== undefined) return cached;
    if (seen.has(name)) return null; // cycle guard, shouldn't happen
    seen.add(name);
    const n = byName.get(name);
    const m = n?.metrics;
    if (!m || m.runs_considered === 0) {
      best.set(name, null);
      return null;
    }
    const own = { value: m.success_rate, length: 1 };
    const ps = parents.get(name) ?? [];
    let incoming: { value: number; length: number } | null = null;
    for (const p of ps) {
      const pv = visit(p, new Set(seen));
      if (!pv) continue;
      if (!incoming || pv.length > incoming.length) incoming = pv;
    }
    const chain = incoming
      ? { value: incoming.value * own.value, length: incoming.length + 1 }
      : own;
    best.set(name, chain);
    return chain;
  };

  let overall: { value: number; length: number } | null = null;
  for (const n of nodes) {
    const v = visit(n.name, new Set());
    if (!v) continue;
    if (!overall || v.length > overall.length) overall = v;
  }
  if (!overall) return null;

  // No edges at all: the "longest chain" is length 1, which isn't
  // useful as "rolled". Fall back to the product across all
  // pipelines — reads as "chance every pipeline held on one push".
  if (edges.length === 0) {
    let value = 1;
    let count = 0;
    for (const n of nodes) {
      const m = n.metrics;
      if (!m || m.runs_considered === 0) continue;
      value *= m.success_rate;
      count++;
    }
    if (count === 0) return null;
    return { value, chainLength: count };
  }

  return { value: overall.value, chainLength: overall.length };
}

// pickBottleneckID scans the VSM for a single worst pipeline to
// flag on the canvas. Ranking combines slow (far above the project
// average for lead time) with unreliable (low success rate). A
// pipeline that's worst-of-both wins over one only slightly off on
// one axis.
export function pickBottleneckID(nodes: VSMNode[]): string | null {
  const withMetrics = nodes.filter(
    (n) => n.metrics && n.metrics.runs_considered >= 3,
  );
  if (withMetrics.length < 2) return null;

  const avgLead =
    withMetrics.reduce(
      (sum, n) => sum + (n.metrics?.lead_time_p50_seconds ?? 0),
      0,
    ) / withMetrics.length;

  let best: { id: string; score: number } | null = null;
  for (const n of withMetrics) {
    const m = n.metrics;
    if (!m) continue;
    const slow = avgLead > 0 ? m.lead_time_p50_seconds / avgLead : 1;
    const unreliable = 1 - m.success_rate;
    if (slow < 1.5 && unreliable < 0.2) continue;
    const score = (slow > 1.5 ? slow - 1 : 0) + unreliable * 2;
    if (!best || score > best.score) {
      best = { id: n.pipeline_id, score };
    }
  }
  return best?.id ?? null;
}
