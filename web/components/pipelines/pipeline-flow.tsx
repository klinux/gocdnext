"use client";

import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { AlertTriangle } from "lucide-react";

import { cn } from "@/lib/utils";
import { PipelineCard } from "@/components/pipelines/pipeline-card";
import { statusTone, type StatusTone } from "@/lib/status";
import type { PipelineEdge, PipelineSummary, RunSummary } from "@/types/api";

type Props = {
  projectSlug: string;
  pipelines: PipelineSummary[];
  edges: PipelineEdge[];
  runs: RunSummary[];
};

// PipelineFlow lays the project's pipelines out as a DAG: every
// upstream-material relationship pushes the downstream pipeline one
// layer deeper. Layers stack top-to-bottom so each layer reads as a
// horizontal row of sibling pipelines, with SVG arrows between rows
// signalling trigger direction. The card visuals live in
// PipelineCard — this component only owns the layout + overlay.
export function PipelineFlow({ projectSlug, pipelines, edges, runs }: Props) {
  const pipelinesByName = useMemo(
    () => new Map(pipelines.map((p) => [p.name, p])),
    [pipelines],
  );
  const layers = useMemo(
    () => buildLayers(pipelines, edges),
    [pipelines, edges],
  );

  const containerRef = useRef<HTMLDivElement>(null);
  const cardRefs = useRef(new Map<string, HTMLElement>());
  const [paths, setPaths] = useState<EdgeGeometry[]>([]);

  // Effectful edges between cards are only drawn for real upstream
  // relationships — layers alone don't imply a connection.
  const renderableEdges = useMemo(() => {
    const names = new Set(pipelines.map((p) => p.name));
    return edges.filter(
      (e) => names.has(e.from_pipeline) && names.has(e.to_pipeline),
    );
  }, [pipelines, edges]);

  useLayoutEffect(() => {
    if (renderableEdges.length === 0) {
      setPaths([]);
      return;
    }
    const compute = () => {
      const container = containerRef.current;
      if (!container) return;
      const cRect = container.getBoundingClientRect();
      const next: EdgeGeometry[] = [];
      for (const e of renderableEdges) {
        const from = cardRefs.current.get(e.from_pipeline);
        const to = cardRefs.current.get(e.to_pipeline);
        if (!from || !to) continue;
        const f = from.getBoundingClientRect();
        const t = to.getBoundingClientRect();
        next.push({
          key: `${e.from_pipeline}->${e.to_pipeline}`,
          fromX: f.left + f.width / 2 - cRect.left,
          fromY: f.bottom - cRect.top,
          toX: t.left + t.width / 2 - cRect.left,
          toY: t.top - cRect.top,
        });
      }
      setPaths(next);
    };
    compute();
    const ro = new ResizeObserver(compute);
    if (containerRef.current) ro.observe(containerRef.current);
    for (const el of cardRefs.current.values()) ro.observe(el);
    window.addEventListener("resize", compute);
    return () => {
      ro.disconnect();
      window.removeEventListener("resize", compute);
    };
  }, [renderableEdges, layers]);

  if (pipelines.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No pipelines yet. Push a YAML to the repo&apos;s config folder or run{" "}
        <code className="font-mono">gocdnext apply</code>.
      </p>
    );
  }

  const alerts = pipelines.filter(isAlerting);
  // Within each DAG layer, push failing/degraded pipelines to the
  // front so the eye lands on what needs attention first. The layer
  // ordering itself stays untouched — it carries architectural
  // meaning (who triggers whom) we shouldn't reshuffle.
  const layersSorted = layers.map((layer) =>
    [...layer].sort((a, b) => {
      const pa = pipelinesByName.get(a);
      const pb = pipelinesByName.get(b);
      const wa = pa ? alertWeight(pa) : 0;
      const wb = pb ? alertWeight(pb) : 0;
      if (wa !== wb) return wb - wa;
      return a.localeCompare(b);
    }),
  );

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
    <div ref={containerRef} className="relative space-y-4">
      {alerts.length > 0 ? (
        <div className="flex flex-wrap items-center gap-2 rounded-md border border-amber-500/40 bg-amber-500/5 px-3 py-2 text-[12px]">
          <AlertTriangle className="size-4 shrink-0 text-amber-600 dark:text-amber-400" aria-hidden />
          <span className="font-medium text-amber-700 dark:text-amber-400">
            {alerts.length === 1
              ? "1 pipeline needs attention:"
              : `${alerts.length} pipelines need attention:`}
          </span>
          <div className="flex flex-wrap items-center gap-1.5">
            {alerts.map((p) => (
              <button
                key={p.id}
                type="button"
                onClick={() => focusCard(p.name)}
                className="inline-flex items-center gap-1.5 rounded-full border border-amber-500/40 bg-card px-2 py-0.5 font-mono text-[11px] hover:bg-amber-500/10"
                title={alertReason(p)}
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
                <span className="text-muted-foreground">{alertReason(p)}</span>
              </button>
            ))}
          </div>
        </div>
      ) : null}

      {paths.length > 0 ? (
        <svg
          aria-hidden
          className="pointer-events-none absolute inset-0 h-full w-full"
        >
          <defs>
            <marker
              id="dag-arrow-head"
              viewBox="0 0 10 10"
              refX="9"
              refY="5"
              markerWidth="6"
              markerHeight="6"
              orient="auto"
            >
              <path
                d="M 0 0 L 10 5 L 0 10 z"
                className="fill-muted-foreground/70"
              />
            </marker>
          </defs>
          {paths.map((p) => (
            <g key={p.key}>
              {/* Source anchor dot keeps the line legibly attached
                  to the card edge even with soft stroke colour. */}
              <circle
                cx={p.fromX}
                cy={p.fromY}
                r={3}
                className="fill-muted-foreground/70"
              />
              <path
                d={orthogonalPath(p)}
                className="fill-none stroke-muted-foreground/70"
                strokeWidth={2}
                strokeLinecap="round"
                strokeLinejoin="round"
                markerEnd="url(#dag-arrow-head)"
              />
            </g>
          ))}
        </svg>
      ) : null}

      {layersSorted.map((layer, layerIdx) => (
        <div
          key={`layer-${layerIdx}`}
          // Extra top padding on downstream layers leaves room for
          // the connecting arc between rows so it doesn't hit the
          // card border.
          className={cn(layerIdx > 0 && "pt-6")}
        >
          <div className="grid gap-3 lg:grid-cols-2 xl:grid-cols-3">
            {layer.map((name) => {
              const pipeline = pipelinesByName.get(name);
              if (!pipeline) return null;
              return (
                <PipelineCard
                  key={pipeline.id}
                  nodeRef={setCardRef(name)}
                  projectSlug={projectSlug}
                  pipeline={pipeline}
                  edges={edges}
                  runs={runs}
                />
              );
            })}
          </div>
        </div>
      ))}
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
  if (p.metrics && p.metrics.runs_considered >= 3 && p.metrics.success_rate < 0.7) {
    return true;
  }
  return false;
}

// alertWeight ranks pipelines for "failing-first" sort within a
// layer. Higher = comes first.
function alertWeight(p: PipelineSummary): number {
  const tone: StatusTone = p.latest_run
    ? statusTone(p.latest_run.status)
    : "neutral";
  if (tone === "failed") return 4;
  if (tone === "canceled") return 3;
  if (p.metrics && p.metrics.runs_considered >= 3 && p.metrics.success_rate < 0.7) {
    return 2;
  }
  if (tone === "running" || tone === "queued") return 1;
  return 0;
}

function alertReason(p: PipelineSummary): string {
  const status = p.latest_run?.status;
  if (status === "failed" || status === "canceled") return status;
  if (p.metrics && p.metrics.runs_considered >= 3) {
    return `${Math.round(p.metrics.success_rate * 100)}%`;
  }
  return "";
}

type EdgeGeometry = {
  key: string;
  fromX: number;
  fromY: number;
  toX: number;
  toY: number;
};

// orthogonalPath routes the connector through the gutter between
// layers using right-angle elbows with rounded corners — never
// across the body of an intermediate card. Layout:
//   1. drop straight from source toward gutter Y
//   2. arc into a horizontal trunk
//   3. travel laterally to the target column
//   4. arc back to vertical
//   5. drop into target top
// Corner radius collapses gracefully when columns are close enough
// that a 10px corner would overshoot the available run.
function orthogonalPath(p: EdgeGeometry): string {
  const dx = p.toX - p.fromX;
  const dy = p.toY - p.fromY;
  // Place the horizontal trunk at the midpoint between layers so
  // both arcs read as symmetric. Clamp to a sensible distance from
  // each card so the line clearly leaves one before entering the
  // next.
  const midY = p.fromY + dy / 2;
  // Same column → straight vertical, no elbows.
  if (Math.abs(dx) < 4) {
    return `M ${p.fromX} ${p.fromY} L ${p.toX} ${p.toY}`;
  }
  const r = Math.min(10, Math.abs(dy) / 2 - 2, Math.abs(dx) / 2 - 2);
  if (r <= 1) {
    // Available run too tight for arcs — degrade to a sharp elbow.
    return `M ${p.fromX} ${p.fromY} L ${p.fromX} ${midY} L ${p.toX} ${midY} L ${p.toX} ${p.toY}`;
  }
  const sweepLeft = dx > 0 ? 1 : 0; // SVG arc sweep flag direction
  const sweepRight = dx > 0 ? 1 : 0;
  const xAfterFirstArc = p.fromX + (dx > 0 ? r : -r);
  const xBeforeSecondArc = p.toX - (dx > 0 ? r : -r);
  return [
    `M ${p.fromX} ${p.fromY}`,
    `L ${p.fromX} ${midY - r}`,
    `A ${r} ${r} 0 0 ${sweepLeft} ${xAfterFirstArc} ${midY}`,
    `L ${xBeforeSecondArc} ${midY}`,
    `A ${r} ${r} 0 0 ${1 - sweepRight} ${p.toX} ${midY + r}`,
    `L ${p.toX} ${p.toY}`,
  ].join(" ");
}

function buildLayers(
  pipelines: PipelineSummary[],
  edges: PipelineEdge[],
): string[][] {
  const names = new Set(pipelines.map((p) => p.name));
  const inDegree = new Map<string, number>();
  const forward = new Map<string, string[]>();
  for (const p of pipelines) {
    inDegree.set(p.name, 0);
    forward.set(p.name, []);
  }
  for (const e of edges) {
    if (!names.has(e.from_pipeline) || !names.has(e.to_pipeline)) continue;
    inDegree.set(e.to_pipeline, (inDegree.get(e.to_pipeline) ?? 0) + 1);
    forward.get(e.from_pipeline)!.push(e.to_pipeline);
  }

  const layer = new Map<string, number>();
  const queue: string[] = [];
  for (const name of inDegree.keys()) {
    if ((inDegree.get(name) ?? 0) === 0) {
      layer.set(name, 0);
      queue.push(name);
    }
  }
  while (queue.length > 0) {
    const u = queue.shift()!;
    for (const v of forward.get(u) ?? []) {
      const next = Math.max(layer.get(v) ?? 0, (layer.get(u) ?? 0) + 1);
      layer.set(v, next);
      inDegree.set(v, (inDegree.get(v) ?? 0) - 1);
      if ((inDegree.get(v) ?? 0) === 0) queue.push(v);
    }
  }
  for (const p of pipelines) {
    if (!layer.has(p.name)) layer.set(p.name, 0);
  }

  const maxLayer = Math.max(0, ...Array.from(layer.values()));
  const out: string[][] = Array.from({ length: maxLayer + 1 }, () => []);
  const sorted = [...pipelines].sort((a, b) => a.name.localeCompare(b.name));
  for (const p of sorted) {
    out[layer.get(p.name) ?? 0]!.push(p.name);
  }
  return out;
}
