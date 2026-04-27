"use client";

import { Fragment } from "react";
import { Check, ChevronRight, Loader2, Minus, TriangleAlert, X } from "lucide-react";

import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import { formatDurationSeconds } from "@/lib/format";
import type { StageColumn } from "@/components/pipelines/pipeline-card-helpers";

type Props = {
  columns: StageColumn[];
};

// PipelineStageStrip is the compact dashboard-view stage indicator:
// one horizontal row of status-coloured chips, no nested job cards.
// Multi-job stages show a (N) count; the success rate pill on the
// right of each chip surfaces flakiness without the operator having
// to expand anything. The full job breakdown lives on the run-detail
// page — this view is "is the pipeline healthy?", not "which job
// inside which stage failed?".
export function PipelineStageStrip({ columns }: Props) {
  if (columns.length === 0) {
    return (
      <p className="px-3 py-3 text-xs text-muted-foreground">
        No stages defined yet.
      </p>
    );
  }
  return (
    <div className="flex flex-wrap items-center gap-x-1 gap-y-1.5 px-3 py-2.5">
      {columns.map((col, i) => {
        const isLast = i === columns.length - 1;
        return (
          <Fragment key={`${col.name}-${i}`}>
            <StageChip column={col} />
            {!isLast ? (
              <ChevronRight
                className="size-3 shrink-0 text-muted-foreground/50"
                aria-hidden
              />
            ) : null}
          </Fragment>
        );
      })}
    </div>
  );
}

function StageChip({ column }: { column: StageColumn }) {
  const tone: StatusTone = column.run ? statusTone(column.run.status) : "neutral";
  const jobCount = column.jobs.length;
  const rate =
    column.stat && column.stat.runs_considered > 0
      ? Math.round(column.stat.success_rate * 100)
      : null;
  const p50 =
    column.stat && column.stat.duration_p50_seconds > 0
      ? formatDurationSeconds(column.stat.duration_p50_seconds)
      : null;

  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-[11px]",
        chipToneClasses[tone],
      )}
      title={[
        `${column.name}${jobCount > 1 ? ` (${jobCount} jobs)` : ""}`,
        column.run?.status,
        p50 ? `p50 ${p50}` : null,
        rate != null ? `${rate}% over ${column.stat?.runs_considered} runs` : null,
      ]
        .filter(Boolean)
        .join(" · ")}
    >
      <ToneIcon tone={tone} className="size-3" />
      <span className="font-mono uppercase tracking-wide">{column.name}</span>
      {jobCount > 1 ? (
        <span className="font-mono tabular-nums opacity-70">{jobCount}</span>
      ) : null}
      {rate != null && rate < 90 ? (
        <span
          className={cn(
            "rounded px-1 font-mono tabular-nums",
            rate >= 70 ? "bg-amber-500/15 text-amber-700 dark:text-amber-400" : "bg-red-500/15 text-red-600 dark:text-red-400",
          )}
        >
          {rate}%
        </span>
      ) : null}
    </span>
  );
}

function ToneIcon({
  tone,
  className,
}: {
  tone: StatusTone;
  className: string;
}) {
  switch (tone) {
    case "success":
      return <Check className={className} aria-hidden strokeWidth={3} />;
    case "failed":
      return <X className={className} aria-hidden strokeWidth={3} />;
    case "running":
      return <Loader2 className={cn(className, "animate-spin")} aria-hidden />;
    case "queued":
    case "warning":
    case "awaiting":
      return <TriangleAlert className={className} aria-hidden />;
    case "canceled":
      return <Minus className={className} aria-hidden strokeWidth={3} />;
    default:
      return (
        <span
          className={cn(
            "inline-block rounded-full bg-current opacity-50",
            className,
          )}
          aria-hidden
        />
      );
  }
}

// chipToneClasses set border + tinted bg per status. Success keeps
// the default border (no green wash) so a healthy pipeline reads
// neutral and only abnormal states pull the eye.
const chipToneClasses: Record<StatusTone, string> = {
  success: "border-border bg-card text-foreground",
  failed: "border-red-500/50 bg-red-500/10 text-red-700 dark:text-red-400",
  running: "border-sky-500/50 bg-sky-500/10 text-sky-700 dark:text-sky-400",
  queued: "border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  warning: "border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  awaiting: "border-amber-500/60 bg-amber-500/15 text-amber-700 dark:text-amber-400",
  canceled: "border-muted-foreground/30 bg-muted/40 text-muted-foreground",
  skipped: "border-dashed border-muted-foreground/30 text-muted-foreground/70",
  neutral: "border-dashed border-border text-muted-foreground",
};
