"use client";

import { useState, useTransition } from "react";
import { Loader2, Trash2 } from "lucide-react";
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
import { deleteGlobalSecret, deleteSecret } from "@/server/actions/secrets";

// Same discriminated-union pattern as SecretDialog — slug is
// required at project scope and forbidden at global scope.
type Props =
  | { scope?: "project"; slug: string; name: string }
  | { scope: "global"; name: string };

// Two-step delete: a small Dialog confirmation before the action fires.
// Secrets aren't recoverable once deleted — extra click is cheap insurance.
export function DeleteSecretButton(props: Props) {
  const [open, setOpen] = useState(false);
  const [pending, startTransition] = useTransition();
  const { name } = props;
  const isGlobal = props.scope === "global";

  function onConfirm() {
    startTransition(async () => {
      const res = isGlobal
        ? await deleteGlobalSecret({ name })
        : await deleteSecret({ slug: props.slug, name });
      if (!res.ok) {
        toast.error(`delete secret: ${res.error}`);
        return;
      }
      toast.success(`Secret ${name} removed`);
      setOpen(false);
    });
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="ghost" size="icon-sm" aria-label={`Delete secret ${name}`}>
            <Trash2 className="h-4 w-4" aria-hidden />
          </Button>
        }
      />
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            Delete <span className="font-mono">{name}</span>?
          </DialogTitle>
          <DialogDescription>
            Jobs referencing this secret will fail at dispatch until you recreate it.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <DialogClose render={<Button variant="outline" type="button">Cancel</Button>} />
          <Button variant="destructive" onClick={onConfirm} disabled={pending}>
            {pending ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> : "Delete"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
