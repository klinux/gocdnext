"use client";

import { Fragment } from "react";
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
import { formatDurationSeconds } from "@/lib/format";
import type {
  MergedJob,
  StageColumn,
} from "@/components/pipelines/pipeline-card-helpers";

type Props = {
  columns: StageColumn[];
};

// PipelineStageStrip lays out a pipeline run as: stage name on top
// (small uppercase title) with a row of circular job badges below
// it. Between adjacent stages a thin DASHED chevron — the link is
// just visual flow, the real architectural connections live between
// pipelines (drawn solid by the DAG overlay above). Borrows the
// project-card pill so the same circle vocabulary applies on every
// surface that shows pipeline jobs.
export function PipelineStageStrip({ columns }: Props) {
  if (columns.length === 0) {
    return (
      <p className="px-3 py-2 text-xs text-muted-foreground">
        No stages defined yet.
      </p>
    );
  }
  return (
    <div className="flex flex-wrap items-start gap-x-1 gap-y-2 px-3 py-2">
      {columns.map((col, i) => {
        const isLast = i === columns.length - 1;
        return (
          <Fragment key={`${col.name}-${i}`}>
            <StageGroup column={col} />
            {!isLast ? <DashedSeparator /> : null}
          </Fragment>
        );
      })}
    </div>
  );
}

function StageGroup({ column }: { column: StageColumn }) {
  const rate =
    column.stat && column.stat.runs_considered > 0
      ? Math.round(column.stat.success_rate * 100)
      : null;
  const showRate = rate != null && rate < 90;
  return (
    <div className="flex min-w-0 flex-col items-start gap-1">
      <div className="flex items-baseline gap-1.5 px-0.5">
        <span className="font-mono text-[9px] font-semibold uppercase tracking-wider text-muted-foreground">
          {column.name}
        </span>
        {showRate ? (
          <span
            className={cn(
              "rounded px-1 font-mono text-[9px] tabular-nums",
              rate >= 70
                ? "bg-amber-500/15 text-amber-700 dark:text-amber-400"
                : "bg-red-500/15 text-red-600 dark:text-red-400",
            )}
            title={`${rate}% over ${column.stat?.runs_considered} runs`}
          >
            {rate}%
          </span>
        ) : null}
      </div>
      <div className="flex flex-wrap items-center gap-1">
        {column.jobs.length === 0 ? (
          <JobCircle status={undefined} label={`${column.name}: empty`} />
        ) : (
          column.jobs.map((job) => (
            <JobCircle
              key={job.key}
              status={job.run?.status}
              label={`${column.name}:${job.name}`}
              durationLabel={formatJobDuration(job)}
            />
          ))
        )}
      </div>
    </div>
  );
}

// DashedSeparator is the implied-flow indicator between stage
// groups inside a single pipeline. Subtle on purpose — the real
// architectural edges (cross-pipeline upstream) get the solid
// curved line treatment in the SVG overlay above.
function DashedSeparator() {
  return (
    <div
      className="flex h-[22px] shrink-0 items-center self-end px-1"
      aria-hidden
    >
      <div className="flex items-center gap-[3px]">
        <span className="size-[3px] rounded-full bg-muted-foreground/40" />
        <span className="size-[3px] rounded-full bg-muted-foreground/40" />
        <span className="size-[3px] rounded-full bg-muted-foreground/40" />
      </div>
    </div>
  );
}

function JobCircle({
  status,
  label,
  durationLabel,
}: {
  status: string | undefined;
  label: string;
  durationLabel?: string | null;
}) {
  const tone: StatusTone = status ? statusTone(status) : "neutral";
  const tooltip = [label, status ?? "not run", durationLabel]
    .filter(Boolean)
    .join(" · ");
  return (
    <span
      title={tooltip}
      aria-label={tooltip}
      className={cn(
        "relative inline-flex size-[20px] shrink-0 items-center justify-center rounded-full border-[1.5px]",
        circleClasses[tone],
        status === "running" &&
          "after:absolute after:inset-[-3px] after:rounded-full after:border-[1.5px] after:border-sky-500 after:content-[''] after:animate-ping",
      )}
    >
      <CircleIcon tone={tone} />
    </span>
  );
}

function CircleIcon({ tone }: { tone: StatusTone }) {
  const cls = "size-[10px]";
  switch (tone) {
    case "success":
      return <Check className={cls} aria-hidden strokeWidth={3} />;
    case "failed":
      return <X className={cls} aria-hidden strokeWidth={3} />;
    case "running":
      return <Loader2 className={cn(cls, "animate-spin")} aria-hidden />;
    case "queued":
    case "warning":
    case "awaiting":
      return <TriangleAlert className={cls} aria-hidden />;
    case "canceled":
      return <Minus className={cls} aria-hidden strokeWidth={3} />;
    case "skipped":
    case "neutral":
    default:
      return <ChevronsRight className={cls} aria-hidden strokeWidth={2.5} />;
  }
}

function formatJobDuration(job: MergedJob): string | null {
  const r = job.run;
  if (!r?.started_at) return null;
  const start = Date.parse(r.started_at);
  const end = r.finished_at ? Date.parse(r.finished_at) : NaN;
  if (Number.isNaN(start)) return null;
  if (Number.isNaN(end)) return null;
  const sec = (end - start) / 1000;
  return formatDurationSeconds(sec);
}

const circleClasses: Record<StatusTone, string> = {
  success:
    "bg-emerald-500/10 border-emerald-500/30 text-emerald-600 dark:text-emerald-400",
  failed: "bg-red-500/10 border-red-500/30 text-red-600 dark:text-red-400",
  running: "bg-sky-500/10 border-sky-500/30 text-sky-600 dark:text-sky-400",
  queued:
    "bg-amber-500/10 border-amber-500/30 text-amber-700 dark:text-amber-400",
  warning:
    "bg-amber-500/10 border-amber-500/30 text-amber-700 dark:text-amber-400",
  awaiting:
    "bg-amber-500/15 border-amber-500/50 text-amber-700 dark:text-amber-400",
  canceled:
    "bg-muted-foreground/10 border-muted-foreground/30 text-muted-foreground",
  skipped:
    "bg-muted-foreground/5 border-muted-foreground/20 text-muted-foreground/70",
  neutral: "bg-muted/40 border-border text-muted-foreground",
};
