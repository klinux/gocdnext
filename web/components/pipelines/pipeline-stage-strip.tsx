"use client";

import { Fragment, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import {
  Check,
  ChevronsRight,
  FileText,
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
import { cancelRun, rerunJob, rerunRun } from "@/server/actions/runs";
import { JobDetailSheet } from "@/components/pipelines/job-detail-sheet.client";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
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
          <Tooltip>
            <TooltipTrigger
              render={
                <span
                  className={cn(
                    "cursor-help rounded px-1 font-mono text-[9px] tabular-nums",
                    rate >= 70
                      ? "bg-amber-500/15 text-amber-700 dark:text-amber-400"
                      : "bg-red-500/15 text-red-600 dark:text-red-400",
                  )}
                />
              }
            >
              {rate}%
            </TooltipTrigger>
            <TooltipContent>
              {rate}% over {column.stat?.runs_considered} runs
            </TooltipContent>
          </Tooltip>
        ) : null}
      </div>
      <div className="flex flex-wrap items-center gap-1.5">
        {column.jobs.length === 0 ? (
          <JobCircle status={undefined} />
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
// groups. Fixed-width so stages stay tight together — the previous
// flex-1 expansion looked stretched on wider cards. Real
// architectural dependencies (cross-pipeline upstream) live in
// the chain stack on the left, not in this dash.
function DashedSeparator() {
  return (
    <div
      className="flex h-[26px] w-7 shrink-0 items-center self-end"
      aria-hidden
    >
      <span className="block h-0 w-full border-t-[1.5px] border-dashed border-muted-foreground/50" />
    </div>
  );
}

// JobNode wraps the JobCircle in a dropdown menu: clicking the
// badge surfaces Status (opens the sheet), Restart (reruns just
// this job, falls back to a full-pipeline rerun when the job
// never executed), and Cancel (cancels the parent run when the
// job is queued/running — there's no per-job cancel endpoint
// today, so the menu item is honest about the scope via the
// confirmation toast).
//
// Hover tooltip on the circle still names the job + status +
// duration so the user has a way to peek without committing to a
// click. Falls back to a plain label when there's no runId yet
// (definition-only views).
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
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const [sheetOpen, setSheetOpen] = useState(false);

  const tooltip = [label, status ?? "not run", duration]
    .filter(Boolean)
    .join(" · ");

  if (!runId || !job.run) {
    return (
      <Tooltip>
        <TooltipTrigger render={<span className="relative inline-flex" />}>
          <JobCircle status={status} />
        </TooltipTrigger>
        <TooltipContent>{tooltip}</TooltipContent>
      </Tooltip>
    );
  }

  const jobRunId = job.run.id;
  const isActive = status === "running" || status === "queued";

  const onRestart = () => {
    startTransition(async () => {
      const res = await rerunJob({ jobRunId });
      if (!res.ok) {
        if (res.status === 409) {
          toast.error(`${job.name} is still active — cancel it first`);
        } else {
          toast.error(`Re-run ${job.name} failed: ${res.error}`);
        }
        return;
      }
      const attempt =
        typeof res.data.attempt === "number" ? res.data.attempt : undefined;
      toast.success(
        attempt != null
          ? `Re-running ${job.name} (attempt ${attempt})`
          : `Re-running ${job.name}`,
      );
      router.refresh();
    });
  };

  const onCancel = () => {
    startTransition(async () => {
      const res = await cancelRun({ runId });
      if (!res.ok) {
        toast.error(`Cancel failed: ${res.error}`);
        return;
      }
      toast.success(`Cancelled run`, {
        description: `Cancelling ${job.name} cancels the rest of run too.`,
      });
      router.refresh();
    });
  };

  return (
    <>
      <DropdownMenu>
        <Tooltip>
          <TooltipTrigger
            render={
              <DropdownMenuTrigger
                disabled={pending}
                className="rounded-full outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
                aria-label={`Actions for ${job.name}`}
              />
            }
          >
            <JobCircle status={status} />
          </TooltipTrigger>
          <TooltipContent>{tooltip}</TooltipContent>
        </Tooltip>
        <DropdownMenuContent align="start" className="w-auto min-w-[180px]">
          <DropdownMenuItem onClick={() => setSheetOpen(true)}>
            <FileText className="size-4" />
            <span>View status</span>
          </DropdownMenuItem>
          <DropdownMenuItem onClick={onRestart} disabled={pending}>
            <RotateCcw
              className={cn("size-4", pending && "animate-spin")}
            />
            <span>Restart job</span>
          </DropdownMenuItem>
          {isActive ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                onClick={onCancel}
                disabled={pending}
                variant="destructive"
              >
                <X className="size-4" />
                <span>Cancel run</span>
              </DropdownMenuItem>
            </>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      <JobDetailSheet
        runId={runId}
        jobId={jobRunId}
        jobName={job.name}
        open={sheetOpen}
        onOpenChange={setSheetOpen}
      />
    </>
  );
}

function JobCircle({ status }: { status: string | undefined }) {
  const tone: StatusTone = status ? statusTone(status) : "neutral";
  return (
    <span
      aria-hidden
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
