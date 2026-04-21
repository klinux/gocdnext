import Link from "next/link";
import type { Route } from "next";
import {
  Check,
  ChevronsRight,
  Loader2,
  Minus,
  TriangleAlert,
  X,
} from "lucide-react";

import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import {
  durationBetween,
  formatDurationSeconds,
} from "@/lib/format";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { TriggerPipelineButton } from "@/components/pipelines/trigger-pipeline-button.client";
import type {
  DefinitionJob,
  JobRunSummaryLite,
  PipelineSummary,
  StageRunSummary,
} from "@/types/api";

type Props = {
  projectSlug: string;
  pipeline: PipelineSummary;
};

// Model one row per stage for the GitLab-style horizontal flow.
// `run` is the matching stage_run when the pipeline has executed;
// `jobs` is the merged job list — runtime job_runs when available,
// falling back to the YAML definition's jobs so never-run stages
// still show their planned shape.
type StageColumn = {
  name: string;
  run?: StageRunSummary;
  jobs: MergedJob[];
};

type MergedJob = {
  key: string;
  name: string;
  run?: JobRunSummaryLite;
};

// PipelineCard renders a GitLab-CI-style pipeline flow: header
// with run metadata on top, horizontal stage boxes with their
// jobs listed vertically inside. Stage order comes from
// DefinitionStages (or stage_runs.ordinal when definition is
// missing); jobs prefer runtime data, falling back to the YAML
// definition so pipelines that never ran still show structure.
export function PipelineCard({ projectSlug, pipeline }: Props) {
  const run = pipeline.latest_run;
  const columns = buildColumns(pipeline);

  return (
    <article className="rounded-lg border bg-card p-5 shadow-sm">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex items-baseline gap-2">
            <h3 className="truncate font-mono text-base font-semibold">
              {pipeline.name}
            </h3>
            <span className="text-xs text-muted-foreground">
              v{pipeline.definition_version}
            </span>
          </div>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
            {run ? (
              <>
                <Link
                  href={`/runs/${run.id}` as Route}
                  className="font-mono text-foreground hover:underline"
                >
                  #{run.counter}
                </Link>
                <StatusBadge status={run.status} className="text-[11px]" />
                <span>
                  {formatDurationSeconds(
                    durationBetween(run.started_at, run.finished_at),
                  )}
                </span>
                <span>
                  · <RelativeTime at={run.started_at ?? run.created_at} />
                </span>
                {run.triggered_by ? (
                  <span>· by {run.triggered_by}</span>
                ) : (
                  <span>· {run.cause}</span>
                )}
              </>
            ) : (
              <span className="italic">
                Never run — push or trigger to start
              </span>
            )}
          </div>
        </div>
        <TriggerPipelineButton
          pipelineId={pipeline.id}
          pipelineName={pipeline.name}
          projectSlug={projectSlug}
        />
      </div>

      {columns.length > 0 ? (
        <div className="mt-4 overflow-x-auto">
          <div className="flex items-stretch gap-2">
            {columns.map((col, i) => (
              <StageColumnBox
                key={`${col.name}-${i}`}
                column={col}
                isLast={i === columns.length - 1}
              />
            ))}
          </div>
        </div>
      ) : (
        <p className="mt-4 text-xs text-muted-foreground">
          No stages defined yet.
        </p>
      )}
    </article>
  );
}

function buildColumns(pipeline: PipelineSummary): StageColumn[] {
  const runStages = pipeline.latest_run_stages ?? [];
  const defStages = pipeline.definition_stages ?? [];
  const defJobs = pipeline.definition_jobs ?? [];

  const runByName = new Map(runStages.map((s) => [s.name, s]));
  const defJobsByStage = groupBy(defJobs, (j) => j.stage);

  const orderedStageNames =
    defStages.length > 0 ? defStages : runStages.map((s) => s.name);

  return orderedStageNames.map((name) => {
    const stageRun = runByName.get(name);
    return {
      name,
      run: stageRun,
      jobs: mergeJobs(stageRun?.jobs ?? [], defJobsByStage.get(name) ?? []),
    };
  });
}

function mergeJobs(
  runtime: JobRunSummaryLite[],
  def: DefinitionJob[],
): MergedJob[] {
  const runtimeByName = new Map(runtime.map((j) => [j.name, j]));
  // Definition order wins when both paths exist — keeps the same
  // job rendering between "about to run" and "just finished" so
  // the list doesn't reshuffle mid-execution.
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
    if (bucket) {
      bucket.push(item);
    } else {
      out.set(k, [item]);
    }
  }
  return out;
}

function StageColumnBox({
  column,
  isLast,
}: {
  column: StageColumn;
  isLast: boolean;
}) {
  const stageTone: StatusTone = column.run
    ? statusTone(column.run.status)
    : "neutral";
  return (
    <div className="flex items-center">
      <div
        className={cn(
          "flex min-w-[180px] flex-col gap-2 rounded-lg border bg-background p-3",
          stageColumnClasses[stageTone],
        )}
      >
        <div className="flex items-center justify-between gap-2">
          <span className="truncate text-xs font-semibold uppercase tracking-wide">
            {column.name}
          </span>
          <StageStatusIcon tone={stageTone} />
        </div>
        {column.jobs.length > 0 ? (
          <ul className="space-y-1">
            {column.jobs.map((j) => (
              <JobRow key={j.key} job={j} />
            ))}
          </ul>
        ) : (
          <p className="text-[11px] italic text-muted-foreground/70">
            No jobs
          </p>
        )}
      </div>
      {!isLast ? (
        <span
          aria-hidden
          className="mx-1 inline-block h-px w-3 shrink-0 bg-muted-foreground/30"
        />
      ) : null}
    </div>
  );
}

function JobRow({ job }: { job: MergedJob }) {
  const tone: StatusTone = job.run ? statusTone(job.run.status) : "neutral";
  const label = job.run
    ? `${job.name} — ${job.run.status}`
    : `${job.name} — not run`;
  return (
    <li
      title={label}
      aria-label={label}
      className={cn(
        "flex items-center gap-2 rounded-md border bg-card px-2 py-1 text-xs",
        jobRowClasses[tone],
      )}
    >
      <span
        className={cn(
          "inline-flex size-4 shrink-0 items-center justify-center rounded-full",
          jobDotClasses[tone],
          job.run?.status === "running" && "animate-pulse",
        )}
      >
        <StageStatusIcon tone={tone} />
      </span>
      <span className="truncate font-mono">{job.name}</span>
    </li>
  );
}

function StageStatusIcon({ tone }: { tone: StatusTone }) {
  const cls = "size-3";
  switch (tone) {
    case "success":
      return <Check className={cls} aria-hidden strokeWidth={3} />;
    case "failed":
      return <X className={cls} aria-hidden strokeWidth={3} />;
    case "running":
      return <Loader2 className={cn(cls, "animate-spin")} aria-hidden />;
    case "queued":
    case "warning":
      return <TriangleAlert className={cls} aria-hidden />;
    case "canceled":
      return <Minus className={cls} aria-hidden strokeWidth={3} />;
    case "skipped":
    case "neutral":
    default:
      return <ChevronsRight className={cls} aria-hidden strokeWidth={2.5} />;
  }
}

// Stage boxes inherit the tone on their border + a light tint so
// the eye can scan the row and spot failure/running stages.
const stageColumnClasses: Record<StatusTone, string> = {
  success: "border-emerald-500/30",
  failed: "border-red-500/40 bg-red-500/5",
  running: "border-sky-500/40 bg-sky-500/5",
  queued: "border-amber-500/30 bg-amber-500/5",
  warning: "border-amber-500/30 bg-amber-500/5",
  canceled: "border-muted-foreground/20",
  skipped: "border-muted-foreground/15",
  neutral: "border-border",
};

// Job rows use subtle tone washes — the coloured dot on the left
// carries most of the signal; over-colouring the row crowds the
// box.
const jobRowClasses: Record<StatusTone, string> = {
  success: "border-border",
  failed: "border-red-500/30 bg-red-500/5",
  running: "border-sky-500/30 bg-sky-500/5",
  queued: "border-amber-500/30 bg-amber-500/5",
  warning: "border-amber-500/30 bg-amber-500/5",
  canceled: "border-border",
  skipped: "border-border",
  neutral: "border-border",
};

const jobDotClasses: Record<StatusTone, string> = {
  success: "bg-emerald-500 text-white",
  failed: "bg-red-500 text-white",
  running: "bg-sky-500 text-white",
  queued: "bg-amber-500 text-white",
  warning: "bg-amber-500 text-white",
  canceled: "bg-muted-foreground/60 text-background",
  skipped: "bg-muted text-muted-foreground border border-muted-foreground/30",
  neutral: "bg-muted text-muted-foreground border border-muted-foreground/30",
};
