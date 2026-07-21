"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { Loader2, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { deleteEnvironment } from "@/server/actions/environments";

type Props = {
  slug: string;
  environmentId: string;
  environmentName: string;
};

// RemoveEnvironment is the admin-only, two-step destructive control on an
// environment card. Confirm hard-deletes the environment AND its whole deploy
// history — the server cascades the revisions and any registered target (which
// is why it's admin-only, tighter than the maintainer canManage). Environments
// are lazy, so a later deploy to the same name re-creates it empty.
export function RemoveEnvironment({
  slug,
  environmentId,
  environmentName,
}: Props) {
  const router = useRouter();
  const [confirming, setConfirming] = useState(false);
  const [pending, startTransition] = useTransition();

  function remove() {
    startTransition(async () => {
      const res = await deleteEnvironment({ slug, environmentId });
      if (!res.ok) {
        toast.error(`Remove ${environmentName}: ${res.error}`);
        return;
      }
      toast.success(`Environment ${environmentName} removed`);
      setConfirming(false);
      router.refresh();
    });
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
