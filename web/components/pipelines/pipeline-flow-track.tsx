"use client";

import { ArrowDown, Network } from "lucide-react";

import { cn } from "@/lib/utils";
import { PipelineRow } from "@/components/pipelines/pipeline-row";
import { statusTone } from "@/lib/status";
import type { FlowGroup } from "@/lib/pipeline-graph";
import type { PipelineEdge, PipelineSummary, RunSummary } from "@/types/api";

type Props = {
  projectSlug: string;
  flow: FlowGroup<PipelineSummary, PipelineEdge>;
  // The full edge + run sets flow through to each row's overview sheet.
  edges: PipelineEdge[];
  runs: RunSummary[];
};

// PipelineFlowTrack renders one dependency chain: a header naming the
// flow + its endpoint path + aggregate health, then the member rows
// stacked in topological order, connected by a rail with an artifact
// label between each pair (e.g. "passes .image").
export function PipelineFlowTrack({ projectSlug, flow, edges, runs }: Props) {
  const { nodes: pipelines } = flow;
  const health = flowHealth(pipelines);

  return (
    <section className="mb-6">
      <header className="flex items-center gap-2 px-1 pb-2.5">
        <Network className="size-4 shrink-0 text-primary" aria-hidden />
        <span className="text-[13px] font-semibold">Flow</span>
        <span className="font-mono text-xs text-muted-foreground">{flow.path}</span>
        <span
          className={cn(
            "ml-auto rounded-full px-2 py-0.5 text-[11px] font-semibold",
            health.tone === "bad" &&
              "bg-red-500/15 text-red-600 dark:text-red-400",
            health.tone === "ok" &&
              "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
            health.tone === "idle" && "bg-muted text-muted-foreground",
          )}
        >
          {health.label}
        </span>
      </header>

      <div className="overflow-hidden rounded-2xl border border-border bg-card">
        {pipelines.map((p, i) => {
          const next = pipelines[i + 1];
          return (
            <div key={p.id}>
              {i > 0 ? (
                <div className="border-t border-border/60" aria-hidden />
              ) : null}
              <PipelineRow
                projectSlug={projectSlug}
                pipeline={p}
                edges={edges}
                runs={runs}
                showRail
              />
              {next ? (
                <EdgeConnector artifact={artifactLabel(flow.edges, p.name, next.name)} />
              ) : null}
            </div>
          );
        })}
      </div>
    </section>
  );
}

// EdgeConnector is the thin row between two pipelines in a chain: a
// continuous rail segment with a downward arrowhead + the artifact
// that flows across the dependency.
function EdgeConnector({ artifact }: { artifact: string | null }) {
  return (
    <div className="grid h-[34px] grid-cols-[46px_1fr] items-center bg-primary/[0.03]">
      <div className="relative flex h-full justify-center">
        <span className="w-0.5 bg-primary/35" aria-hidden />
        <ArrowDown
          className="absolute -bottom-px size-3 text-primary"
          aria-hidden
        />
      </div>
      <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
        <span>passes</span>
        {artifact ? (
          <span className="rounded-full border border-primary/35 bg-primary/10 px-2 py-0.5 font-mono font-semibold text-primary">
            {artifact}
          </span>
        ) : (
          <span className="font-mono">downstream</span>
        )}
      </div>
    </div>
  );
}

// artifactLabel finds the producing stage on the edge between two
// consecutive pipelines and formats it as ".stage" (same language the
// card's relationship pills use). Null when the ordered pair isn't a
// direct edge (e.g. a diamond branch).
function artifactLabel(
  edges: PipelineEdge[],
  from: string,
  to: string,
): string | null {
  const e = edges.find((x) => x.from_pipeline === from && x.to_pipeline === to);
  if (!e) return null;
  return e.stage ? `.${e.stage}` : null;
}

// flowHealth condenses a chain's status into the header pill: any
// failing pipeline dominates, then all-never-run, else healthy.
function flowHealth(pipelines: PipelineSummary[]): {
  tone: "ok" | "bad" | "idle";
  label: string;
} {
  const failing = pipelines.find((p) => {
    const t = p.latest_run ? statusTone(p.latest_run.status) : "neutral";
    return t === "failed" || t === "canceled";
  });
  if (failing) return { tone: "bad", label: `${failing.name} failed` };
  if (pipelines.every((p) => !p.latest_run))
    return { tone: "idle", label: "never run" };
  return { tone: "ok", label: "healthy" };
}
