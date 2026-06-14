"use client";

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
import { rollbackEnvironment } from "@/server/actions/environments";

type Props = {
  slug: string;
  environmentId: string;
  environmentName: string;
  revisionId: string;
  version: string;
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
}: Props) {
  const [open, setOpen] = useState(false);
  const [pending, startTransition] = useTransition();
  const router = useRouter();

  function onConfirm() {
    startTransition(async () => {
      const res = await rollbackEnvironment({
        slug,
        environmentId,
        toRevisionId: revisionId,
      });
      if (!res.ok) {
        toast.error(`Rollback failed: ${res.error}`);
        return;
      }
      toast.success(`Rolling ${environmentName} back to ${version} — watch the run`);
      setOpen(false);
      router.refresh();
    });
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
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
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle className="break-words">
            Roll back {environmentName} to{" "}
            <span className="break-all font-mono">{version}</span>?
          </DialogTitle>
          <DialogDescription className="break-words">
            Re-runs the deploy of{" "}
            <span className="break-all font-mono">{version}</span> — the same
            version ships to {environmentName} again, recorded as a rollback.
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
              "Roll back"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
