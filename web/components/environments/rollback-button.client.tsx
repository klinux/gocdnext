"use client";

import type { ReactElement } from "react";
import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { Loader2, RotateCcw } from "lucide-react";
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
  redeployCurrentEnvironment,
  rollbackEnvironment,
} from "@/server/actions/environments";

type Props = {
  slug: string;
  environmentId: string;
  environmentName: string;
  revisionId?: string;
  version: string;
  action?: "rollback" | "redeploy";
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  trigger?: ReactElement | null;
};

// Re-deploys a past revision's version (the deploy job of that run is
// re-run; its immutable outputs re-resolve the same version). The
// dialog confirms because a rollback ships to a real environment; the
// API is async (202), so success means "started — watch the run".
export function RollbackButton({
  slug,
  environmentId,
  environmentName,
  revisionId,
  version,
  action = "rollback",
  open,
  onOpenChange,
  trigger,
}: Props) {
  const [internalOpen, setInternalOpen] = useState(false);
  const [pending, startTransition] = useTransition();
  const router = useRouter();
  const dialogOpen = open ?? internalOpen;
  const isRedeploy = action === "redeploy";

  function setDialogOpen(next: boolean) {
    if (open === undefined) setInternalOpen(next);
    onOpenChange?.(next);
  }

  function onConfirm() {
    startTransition(async () => {
      const res = isRedeploy
        ? await redeployCurrentEnvironment({ slug, environmentId })
        : revisionId
          ? await rollbackEnvironment({
              slug,
              environmentId,
              toRevisionId: revisionId,
            })
          : { ok: false as const, error: "missing revision id" };
      if (!res.ok) {
        toast.error(`${isRedeploy ? "Redeploy" : "Rollback"} failed: ${res.error}`);
        return;
      }
      toast.success(
        isRedeploy
          ? `Redeploying ${environmentName} at ${version} — watch the run`
          : `Rolling ${environmentName} back to ${version} — watch the run`,
      );
      setDialogOpen(false);
      router.refresh();
    });
  }

  return (
    <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
      {trigger === null ? null : trigger ? (
        <DialogTrigger render={trigger} />
      ) : (
        <DialogTrigger
          render={
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={`Roll back ${environmentName} to ${version}`}
              title="Roll back to this version"
            >
              <RotateCcw className="h-3.5 w-3.5" aria-hidden />
            </Button>
          }
        />
      )}
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle className="break-words">
            {isRedeploy ? "Redeploy" : "Roll back"} {environmentName}{" "}
            {isRedeploy ? "at" : "to"}{" "}
            <span className="break-all font-mono">{version}</span>?
          </DialogTitle>
          <DialogDescription className="break-words">
            {isRedeploy ? (
              <>
                Re-runs the current deploy job for {environmentName}. The server
                resolves the current version at request time, refuses frozen
                environments, and records the new deploy as a normal redeploy.
              </>
            ) : (
              <>
                Re-runs the deploy of{" "}
                <span className="break-all font-mono">{version}</span> — the same
                version ships to {environmentName} again, recorded as a rollback.
              </>
            )}{" "}
            The deploy runs asynchronously; follow its run to see it land.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <DialogClose
            render={
              <Button variant="outline" type="button">
                Cancel
              </Button>
            }
          />
          <Button onClick={onConfirm} disabled={pending}>
            {pending ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              isRedeploy ? "Redeploy" : "Roll back"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
