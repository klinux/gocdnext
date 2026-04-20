"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import { Ban, RotateCcw } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { isTerminalStatus } from "@/lib/status";
import { cancelRun, rerunRun } from "@/server/actions/runs";

type Props = { runId: string; status: string };

// RunActions renders Cancel (while the run is active) and Re-run
// (always). Buttons disable during the round-trip and we pull a
// sonner toast on both success + failure so the operator gets
// confirmation without digging into the run detail table.
export function RunActions({ runId, status }: Props) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const [confirmingCancel, setConfirmingCancel] = useState(false);
  const terminal = isTerminalStatus(status);

  const doCancel = () => {
    setConfirmingCancel(false);
    startTransition(async () => {
      const res = await cancelRun({ runId });
      if (res.ok) {
        toast.success("Run canceled");
        router.refresh();
      } else {
        toast.error(`Cancel failed: ${res.error}`);
      }
    });
  };

  const doRerun = () => {
    startTransition(async () => {
      const res = await rerunRun({ runId });
      if (res.ok) {
        const newID = String(res.data.run_id ?? "");
        toast.success("Run queued", {
          description: newID ? `Counter #${res.data.counter}` : undefined,
          action: newID
            ? {
                label: "Open",
                onClick: () => router.push(`/runs/${newID}` as Route),
              }
            : undefined,
        });
        router.refresh();
      } else {
        toast.error(`Re-run failed: ${res.error}`);
      }
    });
  };

  return (
    <div className="flex items-center gap-2">
      {!terminal ? (
        confirmingCancel ? (
          <>
            <span className="text-xs text-muted-foreground">Cancel this run?</span>
            <Button size="sm" variant="destructive" onClick={doCancel} disabled={pending}>
              Confirm
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setConfirmingCancel(false)}
              disabled={pending}
            >
              No
            </Button>
          </>
        ) : (
          <Button
            size="sm"
            variant="outline"
            onClick={() => setConfirmingCancel(true)}
            disabled={pending}
          >
            <Ban className="size-3.5" aria-hidden />
            Cancel
          </Button>
        )
      ) : null}
      <Button
        size="sm"
        variant="outline"
        onClick={doRerun}
        disabled={pending}
      >
        <RotateCcw className="size-3.5" aria-hidden />
        Re-run
      </Button>
    </div>
  );
}
