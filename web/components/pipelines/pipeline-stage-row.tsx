"use client";

import { Fragment, useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import {
  Check,
  ChevronRight,
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
import { LiveDuration } from "@/components/shared/live-duration";
import type {
  MergedJob,
  StageColumn,
} from "@/components/pipelines/pipeline-card-helpers";

type Props = {
  columns: StageColumn[];
  runId?: string;
};

// PipelineStageRow lays out the "jobs are the cards" design the
// user anchored on: stages are lightweight labels (no border) above
// a vertical stack of job cards. Between stage groups sit timed
// connectors — the gap between finishing stage N and starting
// stage N+1 in the current run.
//
// Wrap semantics: up to 3 stages per row, next stage moves to the
// row below. Within a row each stage points at its right-hand
// neighbour via an arrow + gap time; at row-ends and on the final
// stage the arrow is suppressed (would otherwise point into empty
// space or across a line break).
const STAGES_PER_ROW = 3;

export function PipelineStageRow({ columns, runId }: Props) {
  if (columns.length === 0) {
    return (
      <p className="px-3 py-3 text-xs text-muted-foreground">
        No stages defined yet.
      </p>
    );
  }
  return (
    <div className="grid grid-cols-3 gap-x-0 gap-y-4 px-3 py-3">
      {columns.map((col, i) => {
        const isRowEnd = (i + 1) % STAGES_PER_ROW === 0;
        const isLast = i === columns.length - 1;
        return (
          <Fragment key={`${col.name}-${i}`}>
            <div className="flex min-w-0 items-start gap-0">
              <StageGroup column={col} runId={runId} />
              {!isRowEnd && !isLast ? (
                <StageConnector gapSec={col.gapToNextSec} />
              ) : null}
            </div>
          </Fragment>
        );
      })}
    </div>
  );
}

function StageGroup({ column, runId }: { column: StageColumn; runId?: string }) {
  return (
    <div className="flex min-w-0 flex-1 flex-col gap-2">
      <StageLabel column={column} />
      <div className="flex flex-col gap-1.5">
        {column.jobs.length === 0 ? (
          <p className="rounded-md border border-dashed border-border px-2 py-3 text-center text-[10px] italic text-muted-foreground/70">
            No jobs
          </p>
        ) : (
          column.jobs.map((j) => (
            <JobCard key={j.key} job={j} runId={runId} />
          ))
        )}
      </div>
    </div>
  );
}

// StageLabel is intentionally flat (no border/background) so the
// visual weight sits on the job cards below. Stage name sits next
// to its run status icon; historical stats (p50, pass rate) render
// on a second micro-row when the pipeline has enough terminal runs.
function StageLabel({ column }: { column: StageColumn }) {
  const tone: StatusTone = column.run
    ? statusTone(column.run.status)
    : "neutral";
  const p50 =
    column.stat && column.stat.duration_p50_seconds > 0
      ? formatDurationSeconds(column.stat.duration_p50_seconds)
      : null;
  const rate =
    column.stat && column.stat.runs_considered > 0
      ? Math.round(column.stat.success_rate * 100)
      : null;
  return (
    <div className="flex items-center gap-1.5 px-0.5">
      <span
        className={cn(
          "inline-flex size-3 shrink-0 items-center justify-center rounded-full",
          toneDotClasses[tone],
          column.run?.status === "running" && "animate-pulse",
        )}
        aria-hidden
        title={column.run?.status ?? "not run"}
      >
        <ToneIcon tone={tone} className="size-2" />
      </span>
      <span className="min-w-0 flex-1 truncate text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
        {column.name}
      </span>
      {p50 != null || rate != null ? (
        <span className="ml-auto flex shrink-0 items-center gap-1 text-[9px] tabular-nums text-muted-foreground">
          {p50 ? (
            <span title={`p50 over last ${column.stat?.runs_considered} runs`}>
              p50 {p50}
            </span>
          ) : null}
          {rate != null ? (
            <span
              className={cn(
                "rounded px-1 font-medium",
                rate >= 90
                  ? "bg-emerald-500/10 text-emerald-500"
                  : rate >= 70
                    ? "bg-amber-500/10 text-amber-500"
                    : "bg-red-500/10 text-red-500",
              )}
              title={`${column.stat?.runs_considered ?? 0} terminal runs`}
            >
              ✓ {rate}%
            </span>
          ) : null}
        </span>
      ) : null}
    </div>
  );
}

// StageConnector sits between two stage groups. Aligned vertically
// to the job-card area (not the stage label) so the arrow points
// from the last card of stage N to the first card of stage N+1 —
// that's the motion the reader follows.
function StageConnector({ gapSec }: { gapSec: number | null }) {
  const label = gapSec != null ? formatDurationSeconds(gapSec) : null;
  return (
    <div
      className="flex w-[36px] shrink-0 flex-col items-center justify-start pt-6"
      aria-hidden
    >
      <ChevronRight className="size-4 text-muted-foreground/60" />
      {label ? (
        <span
          className="mt-0.5 font-mono text-[9px] tabular-nums text-muted-foreground"
          title="Wait between stages"
        >
          {label}
        </span>
      ) : null}
    </div>
  );
}

// JobCard is the primary visual unit — each job in the pipeline
// renders as its own bordered card with status dot, name, duration.
// Click = open detail drawer; retry icon top-right. Tone tints the
// left border subtly so the status is legible at a glance without
// shouting over the pipeline layout.
function JobCard({ job, runId }: { job: MergedJob; runId?: string }) {
  const tone: StatusTone = job.run ? statusTone(job.run.status) : "neutral";
  // Jobs in progress compute duration from Date.now(), which
  // drifts between SSR and hydrate and trips a hydration mismatch
  // error. LiveDuration renders the same string on both sides via
  // suppressHydrationWarning + a mounted gate, then ticks on the
  // client. Terminal jobs (finished_at set) render deterministic
  // values either way.
  const showDuration = Boolean(job.run?.started_at);

  return (
    <div
      className={cn(
        "group relative rounded-md border bg-background p-2 transition-colors",
        jobCardClasses[tone],
      )}
    >
      <div className="flex items-center gap-1.5">
        <span
          className={cn(
            "inline-flex size-3 shrink-0 items-center justify-center rounded-full",
            toneDotClasses[tone],
            job.run?.status === "running" && "animate-pulse",
          )}
          aria-hidden
        >
          <ToneIcon tone={tone} className="size-2" />
        </span>
        {runId && job.run ? (
          <JobDetailSheet
            runId={runId}
            jobId={job.run.id}
            jobName={job.name}
            trigger={
              <button
                type="button"
                className="min-w-0 flex-1 truncate text-left font-mono text-[11px] hover:underline"
                title={`Open job details for ${job.name}`}
              >
                {job.name}
              </button>
            }
          />
        ) : (
          <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-muted-foreground">
            {job.name}
          </span>
        )}
        {runId ? (
          <JobRetryButton
            runId={runId}
            jobName={job.name}
            jobRunId={job.run?.id}
          />
        ) : null}
      </div>
      {showDuration ? (
        <LiveDuration
          startedAt={job.run?.started_at}
          finishedAt={job.run?.finished_at}
          className="mt-0.5 block pl-4 font-mono text-[10px] tabular-nums text-muted-foreground"
        />
      ) : null}
    </div>
  );
}

function JobRetryButton({
  runId,
  jobName,
  jobRunId,
}: {
  runId: string;
  jobName: string;
  // When set, clicking reruns JUST this job within the current
  // run (bumps attempt, reuses artefacts). When null (the job has
  // never executed — pure definition row), we fall back to a
  // full-pipeline rerun. The fallback keeps the affordance useful
  // for "project card" views where the latest run happened but
  // some stages never started.
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
      // Never-ran job: rerun the whole pipeline so the user sees
      // something happen instead of a no-op.
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
      aria-label={`Re-run pipeline for ${jobName}`}
      title="Re-run this commit"
      className="inline-flex size-4 shrink-0 items-center justify-center rounded-sm text-muted-foreground opacity-0 transition-all hover:bg-accent hover:text-foreground disabled:opacity-50 group-hover:opacity-100"
    >
      <RotateCcw className={cn("size-2.5", pending && "animate-spin")} />
    </button>
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
      return <TriangleAlert className={className} aria-hidden />;
    case "canceled":
      return <Minus className={className} aria-hidden strokeWidth={3} />;
    case "skipped":
    case "neutral":
    default:
      return (
        <ChevronsRight className={className} aria-hidden strokeWidth={2.5} />
      );
  }
}

// jobCardClasses tint the border + a subtle bg wash. Success stays
// neutral so every card doesn't turn green on a healthy run —
// anything notable (running / failed / queued) is what should pull
// the eye.
const jobCardClasses: Record<StatusTone, string> = {
  success: "border-border hover:border-foreground/20",
  failed: "border-red-500/50 bg-red-500/5",
  running: "border-sky-500/50 bg-sky-500/5",
  queued: "border-amber-500/40 bg-amber-500/5",
  warning: "border-amber-500/40 bg-amber-500/5",
  awaiting: "border-amber-500/60 bg-amber-500/10",
  canceled: "border-muted-foreground/30",
  skipped: "border-muted-foreground/20 opacity-70",
  neutral: "border-dashed border-border",
};

const toneDotClasses: Record<StatusTone, string> = {
  success: "bg-emerald-500 text-white",
  failed: "bg-red-500 text-white",
  running: "bg-sky-500 text-white",
  queued: "bg-amber-500 text-white",
  warning: "bg-amber-500 text-white",
  awaiting: "bg-amber-500 text-white",
  canceled: "bg-muted-foreground/60 text-background",
  skipped: "bg-muted text-muted-foreground border border-muted-foreground/30",
  neutral: "bg-muted text-muted-foreground border border-muted-foreground/30",
};
