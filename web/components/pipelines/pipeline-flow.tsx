"use client";

import { useLayoutEffect, useMemo, useRef, useState } from "react";

import { cn } from "@/lib/utils";
import { PipelineCard } from "@/components/pipelines/pipeline-card";
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

  const setCardRef = (name: string) => (el: HTMLElement | null) => {
    if (el) cardRefs.current.set(name, el);
    else cardRefs.current.delete(name);
  };

  return (
    <div ref={containerRef} className="relative space-y-4">
      {paths.length > 0 ? (
        <svg
          aria-hidden
          className="pointer-events-none absolute inset-0 h-full w-full"
        >
          <defs>
            <marker
              id="dag-arrow-head"
              viewBox="0 0 10 10"
              refX="8"
              refY="5"
              markerWidth="6"
              markerHeight="6"
              orient="auto"
            >
              <path
                d="M 0 0 L 10 5 L 0 10 z"
                className="fill-muted-foreground/60"
              />
            </marker>
          </defs>
          {paths.map((p) => {
            const midY = (p.fromY + p.toY) / 2;
            return (
              <path
                key={p.key}
                d={`M ${p.fromX} ${p.fromY} C ${p.fromX} ${midY}, ${p.toX} ${midY}, ${p.toX} ${p.toY}`}
                className="fill-none stroke-muted-foreground/60"
                strokeWidth={1.5}
                markerEnd="url(#dag-arrow-head)"
              />
            );
          })}
        </svg>
      ) : null}

      {layers.map((layer, layerIdx) => (
        <div
          key={`layer-${layerIdx}`}
          // Extra top padding on downstream layers leaves room for
          // the connecting arc between rows so it doesn't hit the
          // card border.
          className={cn(layerIdx > 0 && "pt-6")}
        >
          <div className="grid gap-3 lg:grid-cols-2">
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

type EdgeGeometry = {
  key: string;
  fromX: number;
  fromY: number;
  toX: number;
  toY: number;
};

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
