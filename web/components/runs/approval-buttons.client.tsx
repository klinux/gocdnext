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
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { approveJob, rejectJob } from "@/server/actions/approvals";

type Props = {
  jobRunID: string;
  runID: string;
  jobName: string;
  description?: string;
  approvers?: string[];
  // heldByFreeze is true when this gate governs an environment currently under a
  // change-freeze (#227). Approving is blocked server-side, so we disable the
  // Approve action (both the trigger AND an already-open dialog's confirm) and
  // explain why, instead of letting the click fail with a 409. Reject stays
  // available (the server permits a reject during a freeze). frozenEnvs names the
  // frozen governed envs for the explanation.
  heldByFreeze?: boolean;
  frozenEnvs?: string[];
};

// freezeReason phrases the "why approval is paused" text for the tooltip + the
// open dialog, naming the env when there is one and collapsing many.
function freezeReason(frozenEnvs?: string[]): string {
  const envs = frozenEnvs ?? [];
  const subject =
    envs.length === 1
      ? `${envs.join(", ")} is`
      : envs.length > 1
        ? `${envs.length} environments are`
        : "an environment is";
  return `Approval is paused while ${subject} frozen — it resumes when unfrozen.`;
}

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
  heldByFreeze,
  frozenEnvs,
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
        heldByFreeze={heldByFreeze}
        frozenEnvs={frozenEnvs}
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
  heldByFreeze,
  frozenEnvs,
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
  // Approve is blocked while a governed env is frozen; Reject never is.
  const blocked = isApprove && !!heldByFreeze;

  const triggerButton = (
    <Button
      size="sm"
      variant={isApprove ? "default" : "outline"}
      className={isApprove ? "" : "text-red-600 hover:text-red-700"}
      disabled={blocked}
    >
      <Icon className="mr-1 h-4 w-4" aria-hidden />
      {isApprove ? "Approve" : "Reject"}
    </Button>
  );

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      {blocked ? (
        // Disabled trigger + a tooltip that still fires on hover/focus (a bare
        // disabled button swallows both, hence the span wrapper). The Dialog is
        // controlled, so a dialog already open when the freeze arrives keeps
        // showing its explanation + disabled confirm below.
        <Tooltip>
          <TooltipTrigger render={<span className="inline-flex" />}>
            {triggerButton}
          </TooltipTrigger>
          <TooltipContent>{freezeReason(frozenEnvs)}</TooltipContent>
        </Tooltip>
      ) : (
        <DialogTrigger render={triggerButton} />
      )}
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
          {blocked ? (
            <p className="text-xs font-medium text-amber-700 dark:text-amber-300">
              {freezeReason(frozenEnvs)}
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
            disabled={pending || blocked}
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
