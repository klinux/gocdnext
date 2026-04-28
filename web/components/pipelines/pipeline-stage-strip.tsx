"use client";

import { Fragment, useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import {
  Check,
  ChevronsRight,
  Loader2,
  Minus,
  RotateCcw,
  TriangleAlert,
  X,
} from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import { formatDurationSeconds } from "@/lib/format";
import { rerunJob, rerunRun } from "@/server/actions/runs";
import { JobDetailSheet } from "@/components/pipelines/job-detail-sheet.client";
import type {
  MergedJob,
  StageColumn,
} from "@/components/pipelines/pipeline-card-helpers";

type Props = {
  columns: StageColumn[];
  runId?: string;
};

// PipelineStageStrip lays out a pipeline run as: stage name on top
// (small uppercase title) with a row of circular job badges below
// it. Between adjacent stages a thin DASHED chevron — the link is
// just visual flow, the real architectural connections live between
// pipelines (drawn solid by the DAG overlay above). Borrows the
// project-card pill so the same circle vocabulary applies on every
// surface that shows pipeline jobs.
export function PipelineStageStrip({ columns, runId }: Props) {
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
            <StageGroup column={col} runId={runId} />
            {!isLast ? <DashedSeparator /> : null}
          </Fragment>
        );
      })}
    </div>
  );
}

function StageGroup({ column, runId }: { column: StageColumn; runId?: string }) {
  const rate =
    column.stat && column.stat.runs_considered > 0
      ? Math.round(column.stat.success_rate * 100)
      : null;
  const showRate = rate != null && rate < 90;
  return (
    <div className="flex shrink-0 flex-col items-start gap-1">
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
      <div className="flex flex-wrap items-center gap-1.5">
        {column.jobs.length === 0 ? (
          <JobCircle status={undefined} label={`${column.name}: empty`} />
        ) : (
          column.jobs.map((job) => (
            <JobNode
              key={job.key}
              job={job}
              stageName={column.name}
              runId={runId}
            />
          ))
        )}
      </div>
    </div>
  );
}

// DashedSeparator is the implied-flow indicator between stage
// groups. flex-1 + min-w-7 = grows to fill the leftover row width,
// so multi-job stages no longer leave dead space between groups
// while narrow viewports still get a sensible minimum. The line
// stays subtle on purpose: real architectural dependencies (cross-
// pipeline upstream) live in the chain stack, not the dash.
function DashedSeparator() {
  return (
    <div
      className="flex h-[26px] min-w-7 flex-1 items-center self-end"
      aria-hidden
    >
      <span className="block h-0 w-full border-t-[1.5px] border-dashed border-muted-foreground/50" />
    </div>
  );
}

// JobNode is the interactive wrapper around a job circle: clicking
// the badge opens the JobDetailSheet (logs + meta + actions); a
// hover-revealed retry button reruns just this job (or the whole
// run if the job never executed). Falls back to a plain label when
// there's no runId yet (definition-only views).
function JobNode({
  job,
  stageName,
  runId,
}: {
  job: MergedJob;
  stageName: string;
  runId?: string;
}) {
  const status = job.run?.status;
  const label = `${stageName}:${job.name}`;
  const duration = formatJobDuration(job);

  const circle = (
    <JobCircle status={status} label={label} durationLabel={duration} />
  );

  if (!runId || !job.run) {
    return (
      <span className="relative inline-flex" title={`${label} · not run`}>
        {circle}
      </span>
    );
  }

  return (
    <span className="group relative inline-flex">
      <JobDetailSheet
        runId={runId}
        jobId={job.run.id}
        jobName={job.name}
        trigger={
          <button
            type="button"
            className="rounded-full outline-none focus-visible:ring-2 focus-visible:ring-ring"
            title={`Open job details for ${job.name}`}
            aria-label={`Open job details for ${job.name}`}
          >
            {circle}
          </button>
        }
      />
      <JobRetryButton runId={runId} jobName={job.name} jobRunId={job.run.id} />
    </span>
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
        "relative inline-flex size-[26px] shrink-0 items-center justify-center rounded-full border-[1.5px]",
        circleClasses[tone],
        status === "running" &&
          "after:absolute after:inset-[-3px] after:rounded-full after:border-[1.5px] after:border-sky-500 after:content-[''] after:animate-ping",
      )}
    >
      <CircleIcon tone={tone} />
    </span>
  );
}

// JobRetryButton sits on top of the circle, hidden until the
// parent JobNode is hovered. Reruns just this job when we have
// its run id; gracefully falls back to a full-pipeline rerun for
// jobs that never executed.
function JobRetryButton({
  runId,
  jobName,
  jobRunId,
}: {
  runId: string;
  jobName: string;
  jobRunId?: string;
}) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const retry = (e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    startTransition(async () => {
      if (jobRunId) {
        const res = await rerunJob({ jobRunId });
        if (!res.ok) {
          if (res.status === 409) {
            toast.error(`${jobName} is still active — cancel it first`);
          } else {
            toast.error(`Re-run ${jobName} failed: ${res.error}`);
          }
          return;
        }
        const attempt =
          typeof res.data.attempt === "number" ? res.data.attempt : undefined;
        toast.success(
          attempt != null
            ? `Re-running ${jobName} (attempt ${attempt})`
            : `Re-running ${jobName}`,
        );
        router.refresh();
        return;
      }
      const res = await rerunRun({ runId });
      if (!res.ok) {
        toast.error(`Re-run failed: ${res.error}`);
        return;
      }
      const newID = String(res.data.run_id ?? "");
      toast.success(`Re-ran pipeline`, {
        action: newID
          ? {
              label: "Open",
              onClick: () => router.push(`/runs/${newID}` as Route),
            }
          : undefined,
      });
    });
  };
  return (
    <button
      type="button"
      onClick={retry}
      disabled={pending}
      aria-label={`Re-run ${jobName}`}
      title="Re-run this job"
      className="absolute -right-1 -top-1 inline-flex size-[14px] shrink-0 items-center justify-center rounded-full border border-border bg-card text-muted-foreground opacity-0 shadow-sm transition-all hover:bg-accent hover:text-foreground disabled:opacity-50 group-hover:opacity-100 focus-visible:opacity-100"
    >
      <RotateCcw className={cn("size-2.5", pending && "animate-spin")} />
    </button>
  );
}

function CircleIcon({ tone }: { tone: StatusTone }) {
  const cls = "size-[12px]";
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
