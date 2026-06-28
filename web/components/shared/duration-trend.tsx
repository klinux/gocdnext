import type { RunSummary } from "@/types/api";

export type DurationPoint = {
  label: string;
  durationSeconds: number;
  status: string;
};

// runDurationPoints turns runs into oldest→newest duration points (finished
// runs only — an in-flight run has no duration yet), capped at the most recent
// `limit`. Ordered by FINISH TIME, not counter: counter is unique per pipeline
// (UNIQUE(pipeline_id, counter)), so the project-wide chart mixing pipelines
// can't sort by it. Pass withPipeline for that aggregate so labels like
// "build #42" stay unambiguous across pipelines.
export function runDurationPoints(
  runs: RunSummary[],
  limit = 30,
  opts?: { withPipeline?: boolean },
): DurationPoint[] {
  return runs
    .filter((r) => r.started_at && r.finished_at)
    .map((r) => ({
      finishedAt: new Date(r.finished_at!).getTime(),
      label: opts?.withPipeline ? `${r.pipeline_name} #${r.counter}` : `#${r.counter}`,
      durationSeconds: Math.max(
        0,
        (new Date(r.finished_at!).getTime() - new Date(r.started_at!).getTime()) / 1000,
      ),
      status: r.status,
    }))
    .sort((a, b) => a.finishedAt - b.finishedAt)
    .slice(-limit)
    .map(({ finishedAt: _finishedAt, ...p }) => p);
}

function median(sorted: number[]): number {
  if (sorted.length === 0) return 0;
  return sorted.length % 2
    ? sorted[(sorted.length - 1) / 2]!
    : (sorted[sorted.length / 2 - 1]! + sorted[sorted.length / 2]!) / 2;
}

export type DurationSummary = {
  median: number;
  min: number;
  max: number;
  values: number[]; // positive durations, oldest→newest
  // deltaPct compares the recent half's median against the prior half's; null
  // when there isn't enough history (< 4 points) to split a window. slower is
  // true when the recent half is slower (a regression — render it red).
  deltaPct: number | null;
  slower: boolean;
};

// durationSummary derives everything the pill/sheet show from the points alone:
// median, fastest/slowest, the positive series, and a window-over-window delta.
// Returns null below 2 positive points (nothing meaningful to chart).
export function durationSummary(points: DurationPoint[]): DurationSummary | null {
  const values = points.map((p) => p.durationSeconds).filter((v) => v > 0);
  if (values.length < 2) return null;
  const sorted = [...values].sort((a, b) => a - b);

  let deltaPct: number | null = null;
  let slower = false;
  if (values.length >= 4) {
    const mid = Math.floor(values.length / 2);
    const prior = median([...values.slice(0, mid)].sort((a, b) => a - b));
    const recent = median([...values.slice(mid)].sort((a, b) => a - b));
    if (prior > 0) {
      deltaPct = Math.round(((recent - prior) / prior) * 100);
      slower = recent > prior;
    }
  }

  return {
    median: median(sorted),
    min: sorted[0]!,
    max: sorted[sorted.length - 1]!,
    values,
    deltaPct,
    slower,
  };
}
