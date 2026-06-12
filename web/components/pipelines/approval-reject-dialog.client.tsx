"use client";

import { ShieldX } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

type Props = {
  jobName: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
  pending?: boolean;
};

// Confirmation dialog for rejecting an approval gate. A native
// confirm() here reads as a browser artifact, not product UI — and
// reject deserves the weight of a real modal: it permanently fails
// the run and skips every downstream stage.
export function ApprovalRejectDialog({
  jobName,
  open,
  onOpenChange,
  onConfirm,
  pending,
}: Props) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <ShieldX className="size-5 text-destructive" aria-hidden />
            Reject {jobName}?
          </DialogTitle>
          <DialogDescription>
            The run fails <strong>permanently</strong> — downstream stages will
            not execute. A fresh run (new push or “Run latest”) is the only way
            forward after a rejection.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={pending}
          >
            Cancel
          </Button>
          <Button variant="destructive" onClick={onConfirm} disabled={pending}>
            Reject
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
