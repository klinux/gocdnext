"use client";

import { useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import { Play } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { triggerPipelineRun } from "@/server/actions/runs";

type Props = {
  pipelineId: string;
  pipelineName: string;
  projectSlug: string;
};

// TriggerPipelineButton kicks a manual run on a pipeline using its
// latest modification. Shown on the project detail page so operators
// can re-run the "tip" without having to push an empty commit.
export function TriggerPipelineButton({ pipelineId, pipelineName, projectSlug }: Props) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();

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

  return (
    <Button size="sm" variant="outline" onClick={onClick} disabled={pending}>
      <Play className="size-3.5" aria-hidden />
      Run latest
    </Button>
  );
}
