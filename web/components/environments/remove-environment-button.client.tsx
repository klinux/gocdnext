"use client";

import {
  cloneElement,
  isValidElement,
  type MouseEvent,
  type ReactElement,
  type ReactNode,
  useState,
  useTransition,
} from "react";
import { useRouter } from "next/navigation";
import { Loader2, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { deleteEnvironment } from "@/server/actions/environments";

type Props = {
  slug: string;
  environmentId: string;
  environmentName: string;
  trigger?: ReactNode;
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
};

// RemoveEnvironment is the admin-only, two-step destructive control for an
// environment. Confirm hard-deletes the environment AND its whole deploy history
// — the server cascades the revisions and any registered target (which is why
// it's admin-only, tighter than the maintainer canManage). Environments are
// lazy, so a later deploy to the same name re-creates it empty.
export function RemoveEnvironment({
  slug,
  environmentId,
  environmentName,
  trigger,
  open: controlledOpen,
  onOpenChange,
}: Props) {
  const router = useRouter();
  const [internalOpen, setInternalOpen] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [pending, startTransition] = useTransition();
  const dialogOpen = controlledOpen ?? internalOpen;

  function setDialogOpen(next: boolean) {
    if (controlledOpen !== undefined) {
      onOpenChange?.(next);
    } else {
      setInternalOpen(next);
    }
    if (!next) setConfirming(false);
  }

  function remove() {
    startTransition(async () => {
      const res = await deleteEnvironment({ slug, environmentId });
      if (!res.ok) {
        toast.error(`Remove ${environmentName}: ${res.error}`);
        return;
      }
      toast.success(`Environment ${environmentName} removed`);
      setDialogOpen(false);
      router.refresh();
    });
  }

  if (trigger || controlledOpen !== undefined) {
    const triggerElement = isValidElement(trigger)
      ? (trigger as ReactElement<{
          onClick?: (event: MouseEvent<HTMLElement>) => void;
        }>)
      : null;
    const renderedTrigger = triggerElement
      ? cloneElement(triggerElement, {
          onClick: (event: MouseEvent<HTMLElement>) => {
            triggerElement.props.onClick?.(event);
            setDialogOpen(true);
          },
        })
      : trigger;

    return (
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        {renderedTrigger}
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Remove {environmentName}?</DialogTitle>
            <DialogDescription>
              This deletes the environment and its deploy history. A later deploy
              to the same name can recreate it empty.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              type="button"
              onClick={() => setDialogOpen(false)}
              disabled={pending}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              type="button"
              onClick={remove}
              disabled={pending}
            >
              {pending ? (
                <Loader2 className="mr-1 size-3.5 animate-spin" aria-hidden />
              ) : null}
              Confirm
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    );
  }

  if (!confirming) {
    return (
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="h-7 text-xs text-muted-foreground hover:text-destructive"
        onClick={() => setConfirming(true)}
      >
        <Trash2 className="mr-1 size-3.5" aria-hidden /> Remove
      </Button>
    );
  }
  return (
    <span className="flex items-center gap-2 text-xs">
      <span className="text-muted-foreground">
        Delete env + all its history?
      </span>
      <Button
        variant="destructive"
        size="sm"
        className="h-7"
        onClick={remove}
        disabled={pending}
      >
        {pending ? (
          <Loader2 className="mr-1 size-3.5 animate-spin" aria-hidden />
        ) : null}
        Confirm
      </Button>
      <Button
        variant="ghost"
        size="sm"
        className="h-7"
        onClick={() => setConfirming(false)}
        disabled={pending}
      >
        Cancel
      </Button>
    </span>
  );
}
