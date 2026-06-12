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
  Server,
  TriangleAlert,
  X,
} from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import { formatDurationSeconds } from "@/lib/format";
import { approveJob, rejectJob } from "@/server/actions/approvals";
import { cancelJob, cancelRun, rerunJob, rerunRun } from "@/server/actions/runs";
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
import type { RunService } from "@/types/api";

type Props = {
  columns: StageColumn[];
  runId?: string;
  // services for the latest run, optional — when present a
  // compact "services" box renders as the FIRST item in the
  // strip (mirrors the Setup column on the run-detail
  // PipelineCanvas, just sized to the card).
  services?: RunService[];
};

// PipelineStageStrip lays out a pipeline run as: stage name on top
// (small uppercase title) with a row of circular job badges below
// it. Between adjacent stages a thin DASHED chevron — the link is
// just visual flow, the real architectural connections live between
// pipelines (drawn solid by the DAG overlay above). Borrows the
// project-card pill so the same circle vocabulary applies on every
// surface that shows pipeline jobs.
export function PipelineStageStrip({ columns, runId, services }: Props) {
  const hasServices = (services?.length ?? 0) > 0;
  if (columns.length === 0 && !hasServices) {
    return (
      <p className="px-3 py-2 text-xs text-muted-foreground">
        No stages defined yet.
      </p>
    );
  }
  return (
    <div className="flex flex-wrap items-stretch gap-x-1 gap-y-2 px-3 py-2">
      {hasServices ? (
        <>
          <ServicesGroup services={services!} />
          {columns.length > 0 ? <DashedSeparator /> : null}
        </>
      ) : null}
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

// servicePillTone collapses the engine's enum into the shared
// StatusTone vocabulary so the same job-circle palette covers
// services. `stopped` is the cleanup-on-terminal path and folds
// into success — v0.6.1's sticky-failed at the store keeps real
// failures visible after cleanup so the fold can't hide a true
// crash. Mirrors the run-detail PipelineCanvas mapping.
function servicePillTone(s: RunService["status"]): StatusTone {
  switch (s) {
    case "ready":
      return "success";
    case "starting":
      return "running";
    case "stopped":
      return "success";
    case "failed":
      return "failed";
    default:
      return "neutral";
  }
}

// ServicesGroup is the per-card analogue of the run-detail's
// Setup column: one circle per service, status colour, hover
// tooltip with name + image + state. Hugs the stage strip's
// visual grammar (same box style, same circle row) so the
// operator's eye reads "services BEFORE stages" without needing
// a label legend. The header label "services" reuses the
// uppercase-tiny style of stage names.
function ServicesGroup({ services }: { services: RunService[] }) {
  return (
    <div className="relative flex shrink-0 flex-col items-start gap-1 rounded-md border border-border/50 bg-muted/20 px-2.5 py-1.5">
      <h4 className="flex items-center gap-1 text-[10px] uppercase tracking-wide text-muted-foreground">
        <Server className="size-3" aria-hidden />
        services
      </h4>
      <div className="flex flex-wrap items-center gap-1">
        {services.map((svc) => (
          <ServiceCircle key={svc.id} service={svc} />
        ))}
      </div>
    </div>
  );
}

function ServiceCircle({ service }: { service: RunService }) {
  const tone = servicePillTone(service.status);
  const className = serviceCircleToneClasses[tone];
  // ARIA: services on the card carry information the operator
  // can't infer from neighbouring text (job circles are paired
  // with the job name; service circles aren't). Render as a real
  // <button> so screen readers announce role + label, keyboard
  // users can Tab onto it, and the tooltip is dismissible.
  const ariaLabel = `Service ${service.name}: ${service.status}`;
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <button
            type="button"
            aria-label={ariaLabel}
            title={ariaLabel}
            className={cn(
              "inline-flex size-4 items-center justify-center rounded-full border focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40 focus-visible:ring-offset-1",
              className,
            )}
          />
        }
      >
        {service.status === "starting" ? (
          <Loader2
            aria-hidden
            className="size-2.5 animate-spin text-sky-600 dark:text-sky-400"
          />
        ) : service.status === "failed" ? (
          <X
            aria-hidden
            className="size-2.5 text-red-600 dark:text-red-400"
          />
        ) : (
          <Check
            aria-hidden
            className="size-2.5 text-emerald-600 dark:text-emerald-400"
          />
        )}
      </TooltipTrigger>
      <TooltipContent className="max-w-xs">
        <p className="font-mono text-xs font-semibold">{service.name}</p>
        <p className="break-all text-[10px] text-muted-foreground">
          {service.image}
        </p>
        <p className="mt-0.5 text-[10px] uppercase tracking-wide">
          {service.status}
        </p>
        {service.error ? (
          <p className="mt-1 break-words text-[10px] text-red-500">
            {service.error}
          </p>
        ) : null}
      </TooltipContent>
    </Tooltip>
  );
}

// Per-tone container classes for the service circle. Kept here
// (not pulled from a shared map) because the existing
// pipelineCircleToneClasses object is private to StageGroup and
// adding a `neutral` row to it would conflict with the
// "no-status-was-set" fallback that map already uses.
const serviceCircleToneClasses: Record<StatusTone, string> = {
  success: "border-emerald-500/40 bg-emerald-500/10",
  failed: "border-red-500/40 bg-red-500/10",
  running: "border-sky-500/40 bg-sky-500/10",
  queued: "border-amber-500/40 bg-amber-500/10",
  warning: "border-amber-500/40 bg-amber-500/10",
  awaiting: "border-amber-500/40 bg-amber-500/10",
  canceled: "border-muted-foreground/30 bg-muted/40",
  skipped: "border-muted-foreground/30 bg-muted/40",
  neutral: "border-border bg-background",
};

function StageGroup({ column, runId }: { column: StageColumn; runId?: string }) {
  const rate =
    column.stat && column.stat.runs_considered > 0
      ? Math.round(column.stat.success_rate * 100)
      : null;
  const showRate = rate != null && rate < 90;
  return (
    // Each stage is a self-contained box: subtle bordered container
    // with title + circles. The dashed separator connects box edges
    // (not title edges), so flakiness badges floating in the corner
    // never push the box wider than its circle row would naturally
    // need — every adjacent dash starts and ends at consistent points.
    <div className="relative flex shrink-0 flex-col items-start gap-1 rounded-md border border-border/50 bg-muted/20 px-2.5 py-1.5">
      {showRate ? (
        <Tooltip>
          <TooltipTrigger
            render={
              <span
                className={cn(
                  "absolute left-full top-1 ml-1 cursor-help rounded px-1 font-mono text-[9px] tabular-nums shadow-sm",
                  rate >= 70
                    ? "bg-amber-500/90 text-white dark:bg-amber-500/80"
                    : "bg-red-500/90 text-white dark:bg-red-500/80",
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
      <span className="font-mono text-[9px] font-semibold uppercase tracking-wider text-muted-foreground">
        {column.name}
      </span>
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
// groups. Fixed 28px so stages stay tight together regardless of
// title-row width — the previous flex stretch made the gap balloon
// when the title carried a flakiness badge.
function DashedSeparator() {
  return (
    <div
      className="flex w-7 shrink-0 items-center self-stretch"
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
  // Approval gates park the run at the stage boundary; surfacing the
  // decision here means an operator scanning the project page can
  // approve without drilling into the run. The server re-checks role
  // + quorum on POST — these items are convenience, not authority.
  const isAwaiting = status === "awaiting_approval";

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

  // onCancelJob targets ONLY this job_run — siblings and the run as
  // a whole keep going. Backed by POST /api/v1/job_runs/{id}/cancel
  // since v0.14.5 (issue #14). Use this for "stop just this one"; if
  // the operator wants to abort everything, the "Cancel whole run"
  // item below does that.
  //
  // Server responses we distinguish (v0.15.1+ deferred-cancel
  // contract):
  //   - 202 status=canceled  signaled=false → queued, DB-flipped
  //     directly to terminal. "Canceled" is honest.
  //   - 202 status=canceling signaled=true  → running, the agent
  //     received the CancelJob frame; container stops on its own
  //     clock. "Canceling…" — the persistent badge on the job card
  //     mirrors the toast.
  //   - 202 status=canceling signaled=false → running, dispatch
  //     missed the agent (session churning) BUT cancel_requested_at
  //     IS stamped. Register replay or reaper finalises shortly.
  //     "Canceling…" — operator sees the intent landed.
  //   - 503 status=dispatch_failed → running, no row written. Job
  //     is STILL running. Surface the error verbatim — calling it
  //     "Canceled" would lie.
  const onCancelJob = () => {
    startTransition(async () => {
      const res = await cancelJob({ jobRunId });
      if (!res.ok) {
        toast.error(`Cancel ${job.name} failed`, {
          description: res.error || "the agent did not receive the cancel signal; try again",
        });
        return;
      }
      const data = res.data as { signaled?: boolean; status?: string };
      const status = data.status ?? "";
      if (status === "canceling") {
        toast.success(`Canceling ${job.name}`, {
          description: data.signaled
            ? "Cancel signal sent to the agent; container will stop shortly."
            : "Cancel requested — will stop when the agent acknowledges.",
        });
      } else {
        toast.success(`Canceled ${job.name}`, {
          description: "Job was queued — removed before it started.",
        });
      }
      router.refresh();
    });
  };

  // onCancelRun keeps the old "tear down the whole run" behaviour
  // for operators who really mean to abort everything. Labeled
  // explicitly so a hurried click on "Cancel job" doesn't take the
  // siblings with it.
  const onCancelRun = () => {
    startTransition(async () => {
      const res = await cancelRun({ runId });
      if (!res.ok) {
        toast.error(`Cancel run failed: ${res.error}`);
        return;
      }
      toast.success(`Cancel requested`, {
        description:
          "Running jobs will stop when the agent acknowledges. Queued jobs are canceled immediately.",
      });
      router.refresh();
    });
  };

  // Approve straight from the project page. Reject keeps a native
  // confirm() (same pattern as destructive actions elsewhere): it
  // permanently fails the run, and a mis-click here is costlier than
  // one extra dialog.
  const onApprove = () => {
    startTransition(async () => {
      const res = await approveJob({ jobRunID: jobRunId, runID: runId });
      if (!res.ok) {
        toast.error(`Approve ${job.name} failed: ${res.error}`);
        return;
      }
      toast.success(`Approved ${job.name}`);
      router.refresh();
    });
  };

  const onReject = () => {
    if (
      !confirm(
        `Reject "${job.name}"? The run fails permanently — downstream stages will not execute.`,
      )
    ) {
      return;
    }
    startTransition(async () => {
      const res = await rejectJob({ jobRunID: jobRunId, runID: runId });
      if (!res.ok) {
        toast.error(`Reject ${job.name} failed: ${res.error}`);
        return;
      }
      toast.success(`Rejected ${job.name}`);
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
          {isAwaiting ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem onClick={onApprove} disabled={pending}>
                <Check className="size-4 text-emerald-500" />
                <span>Approve</span>
              </DropdownMenuItem>
              <DropdownMenuItem
                onClick={onReject}
                disabled={pending}
                variant="destructive"
              >
                <X className="size-4" />
                <span>Reject</span>
              </DropdownMenuItem>
            </>
          ) : null}
          {isActive ? (
            <>
              <DropdownMenuSeparator />
              <DropdownMenuItem
                onClick={onCancelJob}
                disabled={pending}
                variant="destructive"
              >
                <X className="size-4" />
                <span>Cancel job</span>
              </DropdownMenuItem>
              <DropdownMenuItem
                onClick={onCancelRun}
                disabled={pending}
                variant="destructive"
              >
                <X className="size-4" />
                <span>Cancel whole run</span>
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
