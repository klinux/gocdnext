"use client";

import { useState, useTransition, type ReactNode } from "react";
import { useRouter } from "next/navigation";
import { Check, FileText, RotateCcw, X } from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { approveJob, rejectJob } from "@/server/actions/approvals";
import { cancelJob, cancelRun, rerunJob, rerunRun } from "@/server/actions/runs";
import { ApprovalRejectDialog } from "@/components/pipelines/approval-reject-dialog.client";
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
import type { MergedJob } from "@/components/pipelines/pipeline-card-helpers";

type Props = {
  job: MergedJob;
  runId: string;
  tooltip: string;
  // The trigger visual: a JobCircle in the stage strip, a stage box in
  // the flow row. The dropdown + hover tooltip wrap whatever's passed.
  children: ReactNode;
  align?: "start" | "center" | "end";
  triggerClassName?: string;
};

// JobActions is the per-job action surface shared by the pipeline stage
// strip and the flow-listing row: a status-gated dropdown (View status /
// Restart / Approve / Reject / Cancel job / Cancel run) plus the reject
// dialog and the detail+logs sheet. Requires a live job run — callers
// render a plain tooltip for never-run jobs.
export function JobActions({
  job,
  runId,
  tooltip,
  children,
  align = "start",
  triggerClassName = "rounded-full",
}: Props) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const [sheetOpen, setSheetOpen] = useState(false);
  const [rejectOpen, setRejectOpen] = useState(false);

  const jobRunId = job.run?.id ?? "";
  const status = job.run?.status;
  const isActive = status === "running" || status === "queued";
  // Approval gates park the run at the stage boundary; surfacing the
  // decision here lets an operator approve without drilling into the run.
  // The server re-checks role + quorum on POST — these are convenience.
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

  const onCancelJob = () => {
    startTransition(async () => {
      const res = await cancelJob({ jobRunId });
      if (!res.ok) {
        toast.error(`Cancel ${job.name} failed`, {
          description:
            res.error || "the agent did not receive the cancel signal; try again",
        });
        return;
      }
      const data = res.data as { signaled?: boolean; status?: string };
      if ((data.status ?? "") === "canceling") {
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

  const onRejectConfirmed = () => {
    startTransition(async () => {
      const res = await rejectJob({ jobRunID: jobRunId, runID: runId });
      if (!res.ok) {
        toast.error(`Reject ${job.name} failed: ${res.error}`);
        return;
      }
      setRejectOpen(false);
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
                className={cn(
                  "outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50",
                  triggerClassName,
                )}
                aria-label={`Actions for ${job.name}`}
              />
            }
          >
            {children}
          </TooltipTrigger>
          <TooltipContent>{tooltip}</TooltipContent>
        </Tooltip>
        <DropdownMenuContent align={align} className="w-auto min-w-[180px]">
          <DropdownMenuItem onClick={() => setSheetOpen(true)}>
            <FileText className="size-4" />
            <span>View status</span>
          </DropdownMenuItem>
          <DropdownMenuItem onClick={onRestart} disabled={pending}>
            <RotateCcw className={cn("size-4", pending && "animate-spin")} />
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
                onClick={() => setRejectOpen(true)}
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
              <DropdownMenuItem onClick={onCancelJob} disabled={pending} variant="destructive">
                <X className="size-4" />
                <span>Cancel job</span>
              </DropdownMenuItem>
              <DropdownMenuItem onClick={onCancelRun} disabled={pending} variant="destructive">
                <X className="size-4" />
                <span>Cancel whole run</span>
              </DropdownMenuItem>
            </>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      <ApprovalRejectDialog
        jobName={job.name}
        open={rejectOpen}
        onOpenChange={setRejectOpen}
        onConfirm={onRejectConfirmed}
        pending={pending}
      />
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
