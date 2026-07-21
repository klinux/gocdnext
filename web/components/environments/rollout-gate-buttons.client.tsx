"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
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
import { approveRolloutGate, rejectRolloutGate } from "@/server/actions/environments";
import type { DeployWatch } from "@/types/api";

type Props = {
  slug: string;
  revisionId: string;
  gateId: string;
  environment: string;
};

// RolloutGatePrompt is the amber "canary paused · awaiting approval (1/2)" banner + the
// Approve/Reject pair, shown when a step's gate is armed and undecided. gate_id present +
// no gate_decision is the armed-and-open signal.
export function RolloutGatePrompt({
  slug,
  watch,
  environment,
}: {
  slug: string;
  watch: DeployWatch;
  environment: string;
}) {
  if (!watch.gate_id || watch.gate_decision) return null;
  const count = watch.rollout_step_count ?? 0;
  const step =
    count > 0 && watch.gate_paused_step !== undefined
      ? `step ${Math.min(watch.gate_paused_step + 1, count)}/${count}`
      : null;
  const quorum = `${watch.gate_approvals_now ?? 0}/${watch.gate_required ?? 1}`;
  return (
    <div className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2">
      <p className="text-sm">
        <span className="font-medium text-amber-700 dark:text-amber-400">Canary paused</span>
        {step ? <span className="text-muted-foreground"> · {step}</span> : null}
        <span className="text-muted-foreground"> · awaiting approval ({quorum})</span>
      </p>
      <RolloutGateButtons
        slug={slug}
        revisionId={watch.deployment_revision_id}
        gateId={watch.gate_id}
        environment={environment}
      />
    </div>
  );
}

// RolloutGateButtons is the Approve / Reject pair for an armed canary gate (ADR-0001
// Phase 2). Each opens a confirmation dialog and echoes the gate_id — a stale tab voting
// on a superseded step gets a 409 the server surfaces. Reject makes explicit that it
// ABORTS the rollout (traffic → stable), which is not a Git revert.
export function RolloutGateButtons({ slug, revisionId, gateId, environment }: Props) {
  return (
    <div className="flex items-center gap-2">
      <GateDecision verb="approve" slug={slug} revisionId={revisionId} gateId={gateId} environment={environment} />
      <GateDecision verb="reject" slug={slug} revisionId={revisionId} gateId={gateId} environment={environment} />
    </div>
  );
}

function GateDecision({
  verb,
  slug,
  revisionId,
  gateId,
  environment,
}: Props & { verb: "approve" | "reject" }) {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [pending, startTransition] = useTransition();
  const isApprove = verb === "approve";
  const Icon = isApprove ? Check : X;

  function onConfirm() {
    startTransition(async () => {
      const res = isApprove
        ? await approveRolloutGate({ slug, revisionId, gateId })
        : await rejectRolloutGate({ slug, revisionId, gateId });
      if (!res.ok) {
        toast.error(`${verb} ${environment}: ${res.error}`);
        return;
      }
      toast.success(isApprove ? `Approved rollout to ${environment}` : `Aborted rollout to ${environment}`);
      setOpen(false);
      router.refresh();
    });
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button
            size="sm"
            variant={isApprove ? "default" : "outline"}
            className={isApprove ? "h-7 text-xs" : "h-7 text-xs text-red-600 hover:text-red-700"}
          >
            <Icon className="mr-1 size-3.5" aria-hidden />
            {isApprove ? "Approve" : "Reject"}
          </Button>
        }
      />
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            {isApprove ? "Promote" : "Abort"} the canary to{" "}
            <span className="font-mono">{environment}</span>?
          </DialogTitle>
          <DialogDescription>
            {isApprove
              ? "Approving advances the paused canary one step (or completes it once quorum is met)."
              : "Rejecting aborts the rollout — traffic shifts back to the stable version. This does NOT revert Git; the desired version is unchanged, so a re-sync or a corrected commit rolls forward."}
          </DialogDescription>
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
              <Loader2 className="size-4 animate-spin" aria-hidden />
            ) : isApprove ? (
              "Approve"
            ) : (
              "Abort rollout"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
