"use client";

import { useState, useTransition } from "react";
import { Check, Loader2, X } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { approveJob, rejectJob } from "@/server/actions/approvals";

type Props = {
  jobRunID: string;
  runID: string;
  jobName: string;
  description?: string;
  approvers?: string[];
};

// ApprovalButtons renders the two-step Approve / Reject flow for
// an awaiting_approval gate. Kept client-side because it drives a
// Dialog + toast; the server action + revalidatePath handle the
// refresh. Both buttons share the same dialog shell so the UI
// stays consistent whether the operator clicks Approve or Reject.
export function ApprovalButtons({
  jobRunID,
  runID,
  jobName,
  description,
  approvers,
}: Props) {
  return (
    <div className="mt-3 flex flex-wrap items-center gap-2">
      <DecisionDialog
        verb="approve"
        jobRunID={jobRunID}
        runID={runID}
        jobName={jobName}
        description={description}
        approvers={approvers}
      />
      <DecisionDialog
        verb="reject"
        jobRunID={jobRunID}
        runID={runID}
        jobName={jobName}
        description={description}
        approvers={approvers}
      />
    </div>
  );
}

type DecisionProps = Props & { verb: "approve" | "reject" };

function DecisionDialog({
  verb,
  jobRunID,
  runID,
  jobName,
  description,
  approvers,
}: DecisionProps) {
  const [open, setOpen] = useState(false);
  const [pending, startTransition] = useTransition();

  function onConfirm() {
    startTransition(async () => {
      const res =
        verb === "approve"
          ? await approveJob({ jobRunID, runID })
          : await rejectJob({ jobRunID, runID });
      if (!res.ok) {
        toast.error(`${verb} ${jobName}: ${res.error}`);
        return;
      }
      toast.success(
        verb === "approve"
          ? `Approved ${jobName}`
          : `Rejected ${jobName}`,
      );
      setOpen(false);
    });
  }

  const isApprove = verb === "approve";
  const Icon = isApprove ? Check : X;

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button
            size="sm"
            variant={isApprove ? "default" : "outline"}
            className={isApprove ? "" : "text-red-600 hover:text-red-700"}
          >
            <Icon className="mr-1 h-4 w-4" aria-hidden />
            {isApprove ? "Approve" : "Reject"}
          </Button>
        }
      />
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            {isApprove ? "Approve" : "Reject"}{" "}
            <span className="font-mono">{jobName}</span>?
          </DialogTitle>
          <DialogDescription>
            {description
              ? description
              : isApprove
                ? "The gate will transition to success and the next stage starts dispatching."
                : "The gate will fail and the rest of the run is canceled."}
          </DialogDescription>
          {approvers && approvers.length > 0 ? (
            <p className="text-xs text-muted-foreground">
              Approvers:{" "}
              <span className="font-mono">{approvers.join(", ")}</span>
            </p>
          ) : null}
        </DialogHeader>
        <DialogFooter>
          <DialogClose
            render={
              <Button variant="ghost" type="button">
                Cancel
              </Button>
            }
          />
          <Button
            variant={isApprove ? "default" : "destructive"}
            onClick={onConfirm}
            disabled={pending}
          >
            {pending ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : isApprove ? (
              "Approve"
            ) : (
              "Reject"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
