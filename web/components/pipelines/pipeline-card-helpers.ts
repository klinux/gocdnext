import type {
  DefinitionJob,
  JobRunSummaryLite,
  PipelineSummary,
  StageRunSummary,
  StageStat,
} from "@/types/api";

// StageColumn is the unit both the stage strip AND the jobs grid
// draw. Keeping them aligned is easier when they share a single
// ordered list of columns — if a def-only stage exists, it still
// renders a placeholder cell in both rows.
export type StageColumn = {
  name: string;
  run?: StageRunSummary;
  jobs: MergedJob[];
  // Historical stats for this stage — absent when the pipeline is
  // too young to have aggregates (< window of terminal runs).
  stat?: StageStat;
  // Seconds between this stage finishing and the next stage
  // starting in the current run. Null when either timing is
  // missing or it's the last stage.
  gapToNextSec: number | null;
  // Seconds this stage took in the current run (wall clock). Null
  // when not started yet.
  durationSec: number | null;
};

export type MergedJob = {
  key: string;
  name: string;
  run?: JobRunSummaryLite;
};

export function buildColumns(pipeline: PipelineSummary): StageColumn[] {
  const runStages = pipeline.latest_run_stages ?? [];
  const defStages = pipeline.definition_stages ?? [];
  const defJobs = pipeline.definition_jobs ?? [];
  const runByName = new Map(runStages.map((s) => [s.name, s]));
  const defJobsByStage = groupBy(defJobs, (j) => j.stage);
  const statByName = new Map(
    (pipeline.metrics?.stage_stats ?? []).map((s) => [s.name, s]),
  );

  const orderedNames =
    defStages.length > 0 ? defStages : runStages.map((s) => s.name);

  const columns: StageColumn[] = orderedNames.map((name) => {
    const stageRun = runByName.get(name);
    return {
      name,
      run: stageRun,
      jobs: mergeJobs(stageRun?.jobs ?? [], defJobsByStage.get(name) ?? []),
      stat: statByName.get(name),
      durationSec: durationSec(stageRun?.started_at, stageRun?.finished_at),
      gapToNextSec: null,
    };
  });

  // Second pass for gap times: need lookahead into column i+1's
  // start, which the initial map doesn't see without the full
  // list assembled.
  for (let i = 0; i < columns.length - 1; i++) {
    const cur = columns[i]!.run;
    const nxt = columns[i + 1]!.run;
    if (!cur?.finished_at || !nxt?.started_at) continue;
    const sec = secondsBetween(cur.finished_at, nxt.started_at);
    columns[i]!.gapToNextSec = sec != null && sec >= 0 ? sec : null;
  }
  return columns;
}

function mergeJobs(
  runtime: JobRunSummaryLite[],
  def: DefinitionJob[],
): MergedJob[] {
  const runtimeByName = new Map(runtime.map((j) => [j.name, j]));
  if (def.length > 0) {
    return def.map((j) => ({
      key: j.name,
      name: j.name,
      run: runtimeByName.get(j.name),
    }));
  }
  return runtime.map((j) => ({ key: j.id, name: j.name, run: j }));
}

function groupBy<T, K>(items: T[], keyFn: (t: T) => K): Map<K, T[]> {
  const out = new Map<K, T[]>();
  for (const item of items) {
    const k = keyFn(item);
    const bucket = out.get(k);
    if (bucket) bucket.push(item);
    else out.set(k, [item]);
  }
  return out;
}

function durationSec(
  start: string | undefined,
  end: string | undefined,
): number | null {
  if (!start) return null;
  const s = Date.parse(start);
  const e = end ? Date.parse(end) : Date.now();
  if (Number.isNaN(s) || Number.isNaN(e) || e < s) return null;
  return (e - s) / 1000;
}

function secondsBetween(a: string, b: string): number | null {
  const x = Date.parse(a);
  const y = Date.parse(b);
  if (Number.isNaN(x) || Number.isNaN(y)) return null;
  return (y - x) / 1000;
}

// pickBottleneck surfaces the single worst stage in a run, for the
// call-out row. We rank by two independent signals: duration well
// above the stage's own p50, and a low pass rate. A stage that
// trips both flags beats one that trips only one — that way a
// universally-flaky stage outranks a merely-slow one.
export type Bottleneck = {
  stageName: string;
  reason: "slow" | "flaky" | "both";
  overP50Sec?: number;
  successRate?: number;
};

export function pickBottleneck(columns: StageColumn[]): Bottleneck | null {
  let best: (Bottleneck & { score: number }) | null = null;
  for (const col of columns) {
    const p50 = col.stat?.duration_p50_seconds ?? 0;
    const slow =
      col.durationSec != null && p50 > 0 && col.durationSec > p50 * 1.5;
    const flaky =
      col.stat != null &&
      col.stat.runs_considered >= 3 &&
      col.stat.success_rate < 0.7;
    if (!slow && !flaky) continue;
    const overP50 = slow && col.durationSec != null ? col.durationSec - p50 : 0;
    const flakyScore = flaky ? (1 - (col.stat?.success_rate ?? 0)) * 200 : 0;
    const score = overP50 + flakyScore;
    const reason: Bottleneck["reason"] = slow && flaky ? "both" : slow ? "slow" : "flaky";
    if (!best || score > best.score) {
      best = {
        stageName: col.name,
        reason,
        overP50Sec: slow ? overP50 : undefined,
        successRate: flaky ? col.stat?.success_rate : undefined,
        score,
      };
    }
  }
  if (!best) return null;
  const { score: _ignored, ...rest } = best;
  void _ignored;
  return rest;
}
