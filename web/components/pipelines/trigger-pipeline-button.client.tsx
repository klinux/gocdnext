"use client";

import { useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import { Play } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { triggerPipelineRun } from "@/server/actions/runs";

type Props = {
  pipelineId: string;
  pipelineName: string;
  projectSlug: string;
  // currentStatus reflects the latest run's status when known.
  // "running" or "queued" disables the trigger to avoid stacking
  // duplicate runs while the previous one is still in flight —
  // the server would 409 on dispatch anyway, but blocking up front
  // gives the operator clearer feedback than a toast error.
  currentStatus?: string;
};

// TriggerPipelineButton kicks a manual run on a pipeline using its
// latest modification. Shown on the project detail page so operators
// can re-run the "tip" without having to push an empty commit.
export function TriggerPipelineButton({
  pipelineId,
  pipelineName,
  projectSlug,
  currentStatus,
}: Props) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const isActive = currentStatus === "running" || currentStatus === "queued";

  const onClick = () => {
    startTransition(async () => {
      const res = await triggerPipelineRun({ pipelineId, projectSlug });
      if (res.ok) {
        const newID = String(res.data.run_id ?? "");
        toast.success(`${pipelineName} queued`, {
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
        toast.error(`Trigger failed: ${res.error}`);
      }
    });
  };

  const button = (
    <Button
      size="sm"
      variant="outline"
      onClick={onClick}
      disabled={pending || isActive}
      className="h-6 gap-1 rounded-full px-2 text-[11px]"
    >
      <Play className="size-3" aria-hidden />
      Run latest
    </Button>
  );

  if (!isActive) return button;

  return (
    <Tooltip>
      <TooltipTrigger render={<span className="inline-flex" />}>
        {button}
      </TooltipTrigger>
      <TooltipContent>
        {currentStatus === "running"
          ? "A run is already in flight — wait or cancel it first"
          : "A run is already queued"}
      </TooltipContent>
    </Tooltip>
  );
}
