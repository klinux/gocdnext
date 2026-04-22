"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import { ExternalLink, RotateCcw } from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { rerunRun } from "@/server/actions/runs";

type Props = {
  // The click target — a status chip rendered by the caller. This
  // component wraps it in a DropdownMenuTrigger so the job looks
  // identical whether the pipeline has run or not.
  children: React.ReactNode;
  // The full job row's aria-label — propagates to the trigger so
  // screen readers hear "click to open actions for <job>".
  label: string;
  // When the pipeline has run, runId points at the row to link into
  // /runs/{id}. Absent → no "View logs" action, only a disabled
  // hint explaining it's not available yet.
  runId?: string;
  // Optional job_run id. When present, "View logs" appends
  // #job-<id> so the detail page scrolls + highlights the exact
  // job instead of dumping the user at the top of the run.
  jobRunId?: string;
};

// JobActionsMenu is the per-job kebab on the project DAG cards.
// Opens a dropdown with "View logs" (navigates to the run detail
// page) and "Re-run pipeline" (re-uses the existing run rerun
// server action). Re-running a specific job is intentionally not
// on the menu yet — the backend lacks that granularity, and a
// mislabelled action would violate the user's expectation.
export function JobActionsMenu({ children, label, runId, jobRunId }: Props) {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [pending, startTransition] = useTransition();

  const onRerun = () => {
    if (!runId) return;
    startTransition(async () => {
      const res = await rerunRun({ runId });
      if (!res.ok) {
        toast.error(`Re-run failed: ${res.error}`);
        return;
      }
      const newID = String(res.data.run_id ?? "");
      toast.success("Pipeline re-queued", {
        action: newID
          ? {
              label: "Open",
              onClick: () => router.push(`/runs/${newID}` as Route),
            }
          : undefined,
      });
      setOpen(false);
    });
  };

  return (
    <DropdownMenu open={open} onOpenChange={setOpen}>
      <DropdownMenuTrigger
        aria-label={`Actions for ${label}`}
        className={cn(
          "inline-flex cursor-pointer items-center gap-1.5 rounded-sm text-left outline-none hover:bg-accent/40 focus-visible:ring-2 focus-visible:ring-ring",
          "px-1 py-0.5",
        )}
      >
        {children}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-44">
        {runId ? (
          <DropdownMenuItem
            onClick={() => {
              const target = jobRunId
                ? `/runs/${runId}#job-${jobRunId}`
                : `/runs/${runId}`;
              router.push(target as Route);
            }}
            className="whitespace-nowrap"
          >
            <ExternalLink className="size-3.5" />
            View logs
          </DropdownMenuItem>
        ) : (
          <DropdownMenuItem disabled className="whitespace-nowrap">
            <ExternalLink className="size-3.5" />
            View logs (no run yet)
          </DropdownMenuItem>
        )}
        <DropdownMenuSeparator />
        {runId ? (
          <DropdownMenuItem
            onClick={onRerun}
            disabled={pending}
            className="whitespace-nowrap"
          >
            <RotateCcw className="size-3.5" />
            {pending ? "Re-queuing…" : "Re-run this commit"}
          </DropdownMenuItem>
        ) : (
          <DropdownMenuItem disabled className="whitespace-nowrap">
            <RotateCcw className="size-3.5" />
            Trigger pipeline first
          </DropdownMenuItem>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
