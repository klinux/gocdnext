"use client";

import { useState, useTransition } from "react";
import { CheckCircle2, Loader2, Rocket, XOctagon } from "lucide-react";
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
import { RolloutGateButtons } from "@/components/environments/rollout-gate-buttons.client";
import { abortRollout, promoteRollout } from "@/server/actions/environments";
import type { Rollout } from "@/types/api";

type Props = {
  slug: string;
  cluster: string;
  canManage: boolean;
  rollout: Rollout;
  // onActed refreshes the live poll after a successful direct actuation (the gate path
  // reuses RolloutGateButtons, which refreshes itself). Optional so the component is
  // trivially testable without the react-query provider.
  onActed?: () => void;
};

// RolloutActions is the header action cluster (right of the status pill). Three exclusive
// states, server-authoritative:
//   - an armed gate governs the rollout → Approve / Reject (the audited vote path) + quorum;
//     a direct promote/abort is forbidden (the server 409s), so it is never offered here.
//     (Gates arm only on an indefinite CANARY pause, so a blue-green rollout is never
//     gated — it always takes the direct path below.)
//   - no gate, an actionable canary (Paused/Progressing) or blue-green (Paused at
//     pre-promotion), and the viewer may manage → Promote / Abort|Reject confirm dialogs.
//   - otherwise nothing.
export function RolloutActions({ slug, cluster, canManage, rollout, onActed }: Props) {
  const gate = rollout.gate;
  if (gate) {
    return (
      <div className="flex items-center gap-2">
        <span className="font-mono text-[11px] text-muted-foreground">
          awaiting approval ({gate.approvals_now}/{gate.required})
        </span>
        <RolloutGateButtons
          slug={slug}
          revisionId={gate.revision_id}
          gateId={gate.gate_id}
          environment={rollout.name}
        />
      </div>
    );
  }

  // A blue-green rollout only actuates at its pre-promotion pause; a canary also advances
  // while Progressing. Anything else (Healthy/Degraded/aborted, or an unrecognised
  // strategy) offers nothing — the same rev is already live.
  const actionable =
    !rollout.aborted &&
    ((rollout.strategy === "canary" &&
      (rollout.phase === "Paused" || rollout.phase === "Progressing")) ||
      (rollout.strategy === "blueGreen" && rollout.phase === "Paused"));
  if (!actionable || !canManage) return null;

  const strategy = rollout.strategy === "blueGreen" ? "blueGreen" : "canary";
  return (
    <div className="flex items-center gap-2">
      <DirectAction
        verb="promote"
        strategy={strategy}
        slug={slug}
        cluster={cluster}
        rollout={rollout}
        onActed={onActed}
      />
      <DirectAction
        verb="abort"
        strategy={strategy}
        slug={slug}
        cluster={cluster}
        rollout={rollout}
        onActed={onActed}
      />
    </div>
  );
}

// actionCopy is the strategy-aware wording for the direct Promote/Abort dialogs. The
// SERVER action is identical for both strategies (the /status patch promotes/aborts a
// blue-green rollout too — promote swaps the active service, abort discards the preview);
// only the operator-facing copy differs.
function actionCopy(strategy: "canary" | "blueGreen", isPromote: boolean) {
  if (strategy === "blueGreen") {
    return {
      trigger: isPromote ? "Promote" : "Reject",
      subject: "blue-green rollout",
      confirm: isPromote ? "Promote" : "Reject preview",
      past: isPromote ? "Promoted" : "Rejected",
      description: isPromote
        ? "Promotes the preview to active — the active service switches to the new revision."
        : "Discards the preview and keeps the current active revision. This does NOT revert Git.",
      success: isPromote
        ? "the preview is now active"
        : "kept the current active revision",
    };
  }
  return {
    trigger: isPromote ? "Promote" : "Abort",
    subject: "canary",
    confirm: isPromote ? "Promote" : "Abort rollout",
    past: isPromote ? "Promoted" : "Aborted",
    description: isPromote
      ? "Advances the paused canary one step (the controller re-pauses if the next step is another gate)."
      : "Aborts the rollout — traffic shifts back to the stable version. This does NOT revert Git; the desired version is unchanged, so a re-sync or a corrected commit rolls forward.",
    success: isPromote ? "" : "traffic back to stable",
  };
}

function DirectAction({
  verb,
  strategy,
  slug,
  cluster,
  rollout,
  onActed,
}: {
  verb: "promote" | "abort";
  strategy: "canary" | "blueGreen";
  slug: string;
  cluster: string;
  rollout: Rollout;
  onActed?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [pending, startTransition] = useTransition();
  const isPromote = verb === "promote";
  const Icon = isPromote ? Rocket : XOctagon;
  const copy = actionCopy(strategy, isPromote);

  function onConfirm() {
    startTransition(async () => {
      const args = {
        slug,
        cluster,
        namespace: rollout.namespace,
        name: rollout.name,
      };
      const res = isPromote
        ? await promoteRollout(args)
        : await abortRollout(args);
      if (!res.ok) {
        // A forbidden/stale/gated action (403/409) or an unreachable cluster (404) carries
        // a server message — surface it; the action never silently "succeeds".
        toast.error(`${copy.trigger.toLowerCase()} ${rollout.name}: ${res.error}`);
        return;
      }
      toast.success(
        [`${copy.past} ${rollout.name}`, copy.success].filter(Boolean).join(" — "),
      );
      setOpen(false);
      onActed?.();
    });
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button
            size="sm"
            variant={isPromote ? "default" : "outline"}
            className={
              isPromote
                ? "h-7 text-xs"
                : "h-7 text-xs text-red-600 hover:text-red-700"
            }
          >
            <Icon className="mr-1 size-3.5" aria-hidden />
            {copy.trigger}
          </Button>
        }
      />
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            {isPromote ? (
              <CheckCircle2 className="size-4 text-teal-500" aria-hidden />
            ) : (
              <XOctagon className="size-4 text-red-500" aria-hidden />
            )}
            {copy.trigger} the {copy.subject}{" "}
            <span className="font-mono">{rollout.name}</span>?
          </DialogTitle>
          <DialogDescription>{copy.description}</DialogDescription>
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
            variant={isPromote ? "default" : "destructive"}
            onClick={onConfirm}
            disabled={pending}
          >
            {pending ? (
              <Loader2 className="size-4 animate-spin" aria-hidden />
            ) : (
              copy.confirm
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
