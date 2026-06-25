"use client";

import { AlertTriangle } from "lucide-react";

import { cn } from "@/lib/utils";
import { PipelineRow } from "@/components/pipelines/pipeline-row";
import { PipelineFlowTrack } from "@/components/pipelines/pipeline-flow-track";
import { groupByDependency } from "@/lib/pipeline-graph";
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
  // "flow" groups pipelines by dependency chain (tracks + rails);
  // "flat" is one continuous list, no grouping/rails. Defaults to flow.
  view?: "flow" | "flat";
};

// PipelineFlow renders the project's pipelines. In "flow" view it groups
// them by dependency chain — each connected component becomes a track of
// rows linked by a rail naming the artifact passed (build → deploy), and
// unconnected pipelines drop into an "Independent" section. "flat" view
// is the same rows in one alphabetical list with no chain chrome.
export function PipelineFlow({
  projectSlug,
  pipelines,
  edges,
  runs,
  view = "flow",
}: Props) {
  if (pipelines.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No pipelines yet. Push a YAML to the repo&apos;s config folder or run{" "}
        <code className="font-mono">gocdnext apply</code>.
      </p>
    );
  }

  const alerts = pipelines.filter(isAlerting);
  const byName = (a: PipelineSummary, b: PipelineSummary) =>
    a.name.localeCompare(b.name);

  const focusRow = (name: string) => {
    const el = document.getElementById(`pl-row-${name}`);
    if (!el) return;
    el.scrollIntoView({ behavior: "smooth", block: "center" });
    el.classList.add("ring-2", "ring-amber-500/60");
    window.setTimeout(() => el.classList.remove("ring-2", "ring-amber-500/60"), 1500);
  };

  // flat: one continuous track, alphabetical, no grouping or rails.
  if (view === "flat") {
    const all = [...pipelines].sort(byName);
    return (
      <div className="space-y-4">
        <AlertStrip alerts={alerts} onFocus={focusRow} />
        <div className="overflow-hidden rounded-2xl border border-border bg-card">
          {all.map((p, i) => (
            <div key={p.id}>
              {i > 0 ? <div className="border-t border-border/60" aria-hidden /> : null}
              <PipelineRow
                projectSlug={projectSlug}
                pipeline={p}
                edges={edges}
                runs={runs}
                showRail={false}
              />
            </div>
          ))}
        </div>
      </div>
    );
  }

  // flow: dependency chains as tracks + an independent section.
  const { flows, independent } = groupByDependency(
    pipelines,
    edges,
    (p) => p.name,
  );
  const independentSorted = [...independent].sort(byName);

  return (
    <div className="space-y-4">
      <AlertStrip alerts={alerts} onFocus={focusRow} />

      {flows.map((flow) => (
        <PipelineFlowTrack
          key={flow.path}
          projectSlug={projectSlug}
          flow={flow}
          edges={edges}
          runs={runs}
        />
      ))}

      {independentSorted.length > 0 ? (
        <section className="space-y-2.5">
          {flows.length > 0 ? (
            <div className="flex items-center gap-3 px-1">
              <span className="font-mono text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
                Independent pipelines
              </span>
              <span className="h-px flex-1 bg-border" aria-hidden />
            </div>
          ) : null}
          <div className="overflow-hidden rounded-2xl border border-border bg-card">
            {independentSorted.map((p, i) => (
              <div key={p.id}>
                {i > 0 ? (
                  <div className="border-t border-border/60" aria-hidden />
                ) : null}
                <PipelineRow
                  projectSlug={projectSlug}
                  pipeline={p}
                  edges={edges}
                  runs={runs}
                  showRail={false}
                />
              </div>
            ))}
          </div>
        </section>
      ) : null}

      {flows.length > 0 ? <FlowLegend /> : null}
    </div>
  );
}

// AlertStrip is the top "needs attention" banner: failing + flaky
// pipelines as chips that scroll their row into view on click.
function AlertStrip({
  alerts,
  onFocus,
}: {
  alerts: PipelineSummary[];
  onFocus: (name: string) => void;
}) {
  if (alerts.length === 0) return null;
  return (
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
                  onClick={() => onFocus(p.name)}
                  className="inline-flex items-center gap-1.5 rounded-full border border-amber-500/40 bg-card px-2 py-0.5 font-mono text-[11px] hover:bg-amber-500/10"
                />
              }
            >
              <span
                className={cn(
                  "size-1.5 rounded-full",
                  p.latest_run?.status === "failed" ? "bg-red-500" : "bg-amber-500",
                )}
                aria-hidden
              />
              {p.name}
              <span className="text-muted-foreground">{alertReason(p)}</span>
            </TooltipTrigger>
            <TooltipContent>Scroll to {p.name}</TooltipContent>
          </Tooltip>
        ))}
      </div>
    </div>
  );
}

// FlowLegend explains the rail + stage swatches at the foot of the list.
function FlowLegend() {
  return (
    <div className="flex flex-wrap items-center gap-6 border-t border-border pt-3 font-mono text-[11px] text-muted-foreground">
      <span className="inline-flex items-center gap-2">
        <span className="h-3 w-0.5 bg-primary/35" aria-hidden />
        rail = dependency chain (upstream → downstream)
      </span>
      <span className="inline-flex items-center gap-1.5">
        <span className="size-2.5 rounded border border-emerald-500 bg-emerald-500/15" aria-hidden />
        passed
      </span>
      <span className="inline-flex items-center gap-1.5">
        <span className="size-2.5 rounded border border-red-500 bg-red-500/15" aria-hidden />
        failed
      </span>
      <span className="inline-flex items-center gap-1.5">
        <span className="size-2.5 rounded border border-muted-foreground/40 bg-muted" aria-hidden />
        waiting
      </span>
    </div>
  );
}

// isAlerting decides whether a pipeline shows up in the top alert strip.
// Failing/canceled latest runs always count; pipelines with a healthy
// latest run but a low historical pass rate do too — flaky CI is "needs
// attention" even when today happens to be green.
function isAlerting(p: PipelineSummary): boolean {
  const tone: StatusTone = p.latest_run ? statusTone(p.latest_run.status) : "neutral";
  if (tone === "failed" || tone === "canceled") return true;
  if (p.metrics && p.metrics.runs_considered >= 3 && p.metrics.success_rate < 0.7) {
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
