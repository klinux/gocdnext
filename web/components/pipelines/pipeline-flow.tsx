"use client";

import { useRef } from "react";
import { AlertTriangle } from "lucide-react";

import { cn } from "@/lib/utils";
import { PipelineCard } from "@/components/pipelines/pipeline-card";
import { statusTone, type StatusTone } from "@/lib/status";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import type { PipelineEdge, PipelineSummary, RunSummary } from "@/types/api";

type Props = {
  projectSlug: string;
  pipelines: PipelineSummary[];
  edges: PipelineEdge[];
  runs: RunSummary[];
};

// PipelineFlow renders all pipelines as a flat grid sorted by alert
// weight (failing → degraded → healthy). Cross-pipeline dependencies
// are NOT drawn as visual lines anymore — every downstream pipeline
// surfaces its triggers via the upstream pill in its footer instead.
// Single source of truth, no overlay/measurement machinery, no
// orphan-edge layout puzzles.
export function PipelineFlow({ projectSlug, pipelines, edges, runs }: Props) {
  const cardRefs = useRef(new Map<string, HTMLElement>());

  if (pipelines.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No pipelines yet. Push a YAML to the repo&apos;s config folder or run{" "}
        <code className="font-mono">gocdnext apply</code>.
      </p>
    );
  }

  // Stable alphabetical order. Sorting by alert weight made cards
  // jump around the grid every time a run kicked off (running →
  // success), which felt like a page reload — disorienting right
  // after the user clicked Run. Failing pipelines still surface in
  // the alert strip above so they're not lost.
  const sortedPipelines = [...pipelines].sort((a, b) =>
    a.name.localeCompare(b.name),
  );

  const alerts = pipelines.filter(isAlerting);

  const setCardRef = (name: string) => (el: HTMLElement | null) => {
    if (el) cardRefs.current.set(name, el);
    else cardRefs.current.delete(name);
  };

  const focusCard = (name: string) => {
    const el = cardRefs.current.get(name);
    if (el) {
      el.scrollIntoView({ behavior: "smooth", block: "center" });
      el.classList.add("ring-2", "ring-amber-500/60");
      window.setTimeout(
        () => el.classList.remove("ring-2", "ring-amber-500/60"),
        1500,
      );
    }
  };

  return (
    <div className="space-y-4">
      {alerts.length > 0 ? (
        <div className="flex flex-wrap items-center gap-2 rounded-md border border-amber-500/40 bg-amber-500/5 px-3 py-2 text-[12px]">
          <AlertTriangle
            className="size-4 shrink-0 text-amber-600 dark:text-amber-400"
            aria-hidden
          />
          <span className="font-medium text-amber-700 dark:text-amber-400">
            {alerts.length === 1
              ? "1 pipeline needs attention:"
              : `${alerts.length} pipelines need attention:`}
          </span>
          <div className="flex flex-wrap items-center gap-1.5">
            {alerts.map((p) => (
              <Tooltip key={p.id}>
                <TooltipTrigger
                  render={
                    <button
                      type="button"
                      onClick={() => focusCard(p.name)}
                      className="inline-flex items-center gap-1.5 rounded-full border border-amber-500/40 bg-card px-2 py-0.5 font-mono text-[11px] hover:bg-amber-500/10"
                    />
                  }
                >
                  <span
                    className={cn(
                      "size-1.5 rounded-full",
                      p.latest_run?.status === "failed"
                        ? "bg-red-500"
                        : "bg-amber-500",
                    )}
                    aria-hidden
                  />
                  {p.name}
                  <span className="text-muted-foreground">
                    {alertReason(p)}
                  </span>
                </TooltipTrigger>
                <TooltipContent>Scroll to {p.name}</TooltipContent>
              </Tooltip>
            ))}
          </div>
        </div>
      ) : null}

      <div className="grid items-start gap-3 lg:grid-cols-2 xl:grid-cols-3">
        {sortedPipelines.map((p) => (
          <div key={p.id} ref={(el) => setCardRef(p.name)(el)}>
            <PipelineCard
              projectSlug={projectSlug}
              pipeline={p}
              edges={edges}
              runs={runs}
            />
          </div>
        ))}
      </div>
    </div>
  );
}

// isAlerting decides whether a pipeline shows up in the top alert
// strip. Failing/canceled latest runs always count; pipelines with
// healthy latest runs but a low historical pass rate do too — flaky
// CI is "needs attention" even when today happens to be green.
function isAlerting(p: PipelineSummary): boolean {
  const tone: StatusTone = p.latest_run
    ? statusTone(p.latest_run.status)
    : "neutral";
  if (tone === "failed" || tone === "canceled") return true;
  if (
    p.metrics &&
    p.metrics.runs_considered >= 3 &&
    p.metrics.success_rate < 0.7
  ) {
    return true;
  }
  return false;
}

function alertReason(p: PipelineSummary): string {
  const status = p.latest_run?.status;
  if (status === "failed" || status === "canceled") return status;
  if (p.metrics && p.metrics.runs_considered >= 3) {
    return `${Math.round(p.metrics.success_rate * 100)}%`;
  }
  return "";
}
