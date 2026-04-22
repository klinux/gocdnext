"use client";

import { Fragment, useLayoutEffect, useMemo, useRef, useState } from "react";

import type { ProjectVSM, VSMNode as VSMNodeT } from "@/types/api";
import { PipelineNode } from "@/components/vsm/pipeline-node.client";
import {
  VSMMetricsStrip,
  pickBottleneckID,
} from "@/components/vsm/vsm-metrics-strip";

// Gap between columns of pipelines (depth layers) and between
// stacked rows when a column has multiple siblings. Edges are
// routed through the gap so the arrows have breathing room.
const COL_GAP = 48;
const ROW_GAP = 16;

type Props = {
  vsm: ProjectVSM;
};

// VSMGraph lays the project's pipelines as a left-to-right DAG.
// Pure HTML/CSS for node positioning + a single absolute SVG
// overlay for arrows between cards (computed via refs + a
// ResizeObserver, same pattern as the Pipelines tab uses for the
// project-level flow). No zoom, no pan, no floating controls —
// the view is a static info display. Overflow scrolls horizontally
// when the DAG is wider than the viewport.
export function VSMGraph({ vsm }: Props) {
  const bottleneckID = useMemo(() => pickBottleneckID(vsm.nodes), [vsm.nodes]);
  const layers = useMemo(() => buildLayers(vsm), [vsm]);
  const windowDays = useMemo(() => firstMetricsWindow(vsm.nodes), [vsm.nodes]);

  const containerRef = useRef<HTMLDivElement>(null);
  const nodeRefs = useRef(new Map<string, HTMLDivElement>());
  const [paths, setPaths] = useState<EdgePath[]>([]);

  const renderableEdges = useMemo(
    () =>
      vsm.edges.filter(
        (e) =>
          nameExists(vsm.nodes, e.from_pipeline) &&
          nameExists(vsm.nodes, e.to_pipeline),
      ),
    [vsm.edges, vsm.nodes],
  );

  // Implicit sequence edges: for the single-layer case (no upstream
  // deps anywhere), connect horizontally-adjacent pipelines with a
  // dashed arrow labeled with the source's p50 duration. Reads as
  // "everyone ran in parallel, and here's how long each took" —
  // no fake dependency claim. Skipped when real edges exist to
  // avoid doubling up on a pair.
  const implicitEdges = useMemo(() => {
    if (layers.length !== 1) return [];
    const row = layers[0]!;
    if (row.length < 2) return [];
    const realPairs = new Set(
      renderableEdges.map((e) => `${e.from_pipeline}->${e.to_pipeline}`),
    );
    const out: { fromID: string; toID: string; sourcePipelineID: string }[] = [];
    for (let i = 0; i < row.length - 1; i++) {
      const from = row[i]!;
      const to = row[i + 1]!;
      if (realPairs.has(`${from.name}->${to.name}`)) continue;
      out.push({
        fromID: from.pipeline_id,
        toID: to.pipeline_id,
        sourcePipelineID: from.pipeline_id,
      });
    }
    return out;
  }, [layers, renderableEdges]);

  // Slowest pipeline across the VSM — the "offender" whose outgoing
  // implicit edge is tinted amber so the eye lands on it first.
  // Top-1 only; ties resolve by the first encountered.
  const slowestPipelineID = useMemo(() => {
    let best: { id: string; p50: number } | null = null;
    for (const n of vsm.nodes) {
      const p50 = n.metrics?.lead_time_p50_seconds;
      if (p50 == null || p50 <= 0) continue;
      if (!best || p50 > best.p50) best = { id: n.pipeline_id, p50 };
    }
    return best?.id ?? null;
  }, [vsm.nodes]);

  const p50ByID = useMemo(() => {
    const m = new Map<string, number>();
    for (const n of vsm.nodes) {
      if (n.metrics && n.metrics.lead_time_p50_seconds > 0) {
        m.set(n.pipeline_id, n.metrics.lead_time_p50_seconds);
      }
    }
    return m;
  }, [vsm.nodes]);

  useLayoutEffect(() => {
    if (renderableEdges.length === 0 && implicitEdges.length === 0) {
      setPaths([]);
      return;
    }
    const byName = new Map(vsm.nodes.map((n) => [n.name, n.pipeline_id]));
    const compute = () => {
      const container = containerRef.current;
      if (!container) return;
      const cRect = container.getBoundingClientRect();
      const next: EdgePath[] = [];
      for (const e of renderableEdges) {
        const fromID = byName.get(e.from_pipeline);
        const toID = byName.get(e.to_pipeline);
        if (!fromID || !toID) continue;
        const from = nodeRefs.current.get(fromID);
        const to = nodeRefs.current.get(toID);
        if (!from || !to) continue;
        const f = from.getBoundingClientRect();
        const t = to.getBoundingClientRect();
        next.push({
          key: `real-${e.from_pipeline}->${e.to_pipeline}`,
          kind: "real",
          stage: e.stage,
          waitSec: e.wait_time_p50_seconds,
          fromX: f.right - cRect.left + container.scrollLeft,
          fromY: f.top + f.height / 2 - cRect.top + container.scrollTop,
          toX: t.left - cRect.left + container.scrollLeft,
          toY: t.top + t.height / 2 - cRect.top + container.scrollTop,
        });
      }
      for (const ie of implicitEdges) {
        const from = nodeRefs.current.get(ie.fromID);
        const to = nodeRefs.current.get(ie.toID);
        if (!from || !to) continue;
        const f = from.getBoundingClientRect();
        const t = to.getBoundingClientRect();
        next.push({
          key: `implicit-${ie.fromID}->${ie.toID}`,
          kind: "implicit",
          sourceP50Sec: p50ByID.get(ie.sourcePipelineID),
          isOffender: ie.sourcePipelineID === slowestPipelineID,
          fromX: f.right - cRect.left + container.scrollLeft,
          fromY: f.top + f.height / 2 - cRect.top + container.scrollTop,
          toX: t.left - cRect.left + container.scrollLeft,
          toY: t.top + t.height / 2 - cRect.top + container.scrollTop,
        });
      }
      setPaths(next);
    };
    compute();
    const ro = new ResizeObserver(compute);
    if (containerRef.current) ro.observe(containerRef.current);
    for (const el of nodeRefs.current.values()) ro.observe(el);
    window.addEventListener("resize", compute);
    return () => {
      ro.disconnect();
      window.removeEventListener("resize", compute);
    };
  }, [
    renderableEdges,
    implicitEdges,
    p50ByID,
    slowestPipelineID,
    vsm.nodes,
    layers,
  ]);

  const setNodeRef = (id: string) => (el: HTMLDivElement | null) => {
    if (el) nodeRefs.current.set(id, el);
    else nodeRefs.current.delete(id);
  };

  if (vsm.nodes.length === 0) {
    return (
      <div className="flex flex-col">
        <div className="rounded-md border border-dashed border-border p-10 text-center text-sm text-muted-foreground">
          This project has no pipelines yet.
        </div>
      </div>
    );
  }

  return (
    <div className="flex flex-col">
      <div
        ref={containerRef}
        className="relative overflow-x-auto bg-muted/10 p-6"
      >
        {paths.length > 0 ? (
          <svg
            aria-hidden
            className="pointer-events-none absolute inset-0"
            style={{
              width: containerRef.current?.scrollWidth ?? "100%",
              height: containerRef.current?.scrollHeight ?? "100%",
            }}
          >
            <defs>
              <marker
                id="vsm-arrow-head"
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
              <marker
                id="vsm-arrow-head-muted"
                viewBox="0 0 10 10"
                refX="9"
                refY="5"
                markerWidth="5"
                markerHeight="5"
                orient="auto"
              >
                <path
                  d="M 0 0 L 10 5 L 0 10 z"
                  className="fill-muted-foreground/30"
                />
              </marker>
            </defs>
            {paths.map((p) => {
              const dx = Math.max(40, (p.toX - p.fromX) / 2);
              const midX = (p.fromX + p.toX) / 2;
              const midY = (p.fromY + p.toY) / 2;
              const isImplicit = p.kind === "implicit";
              return (
                <g key={p.key}>
                  <path
                    d={`M ${p.fromX} ${p.fromY} C ${p.fromX + dx} ${p.fromY}, ${p.toX - dx} ${p.toY}, ${p.toX} ${p.toY}`}
                    className={
                      isImplicit
                        ? "fill-none stroke-muted-foreground/30"
                        : "fill-none stroke-muted-foreground/70"
                    }
                    strokeWidth={isImplicit ? 1 : 1.5}
                    strokeDasharray={isImplicit ? "4 4" : undefined}
                    markerEnd={
                      isImplicit
                        ? "url(#vsm-arrow-head-muted)"
                        : "url(#vsm-arrow-head)"
                    }
                  />
                  {/* Real edge (upstream dep): stage on top, wait
                      time below. Implicit edge (sequence-only): p50
                      of source pipeline — amber when it's the
                      slowest in the grid so the offender pops. */}
                  {p.kind === "real" && p.stage ? (
                    <text
                      x={midX}
                      y={p.waitSec != null ? midY - 12 : midY - 4}
                      textAnchor="middle"
                      className="fill-muted-foreground text-[10px]"
                    >
                      {p.stage}
                    </text>
                  ) : null}
                  {p.kind === "real" && p.waitSec != null ? (
                    <text
                      x={midX}
                      y={midY - 2}
                      textAnchor="middle"
                      className="fill-foreground/70 font-mono text-[10px]"
                    >
                      {formatWait(p.waitSec)}
                    </text>
                  ) : null}
                  {p.kind === "implicit" && p.sourceP50Sec != null ? (
                    <text
                      x={midX}
                      y={midY - 4}
                      textAnchor="middle"
                      className={
                        p.isOffender
                          ? "fill-amber-600 font-mono text-[10px] font-semibold dark:fill-amber-400"
                          : "fill-muted-foreground/70 font-mono text-[10px]"
                      }
                    >
                      {formatWait(p.sourceP50Sec)}
                    </text>
                  ) : null}
                </g>
              );
            })}
          </svg>
        ) : null}

        {/* Outer flex with min-w-full + justify-center: when the
            DAG is narrower than the viewport it centres; when it's
            wider the overflow on containerRef scrolls horizontally
            — no scaling, no fitView surprises. */}
        <div className="relative flex min-w-full justify-center">
          <div
            className="inline-flex items-start"
            style={{ gap: `${COL_GAP}px` }}
          >
            {layers.length === 1 ? (
              // No dependencies: render the single layer as a
              // horizontal row so the VSM reads left-to-right (like
              // parallel tracks from one push) instead of stacking
              // vertically in a lone column.
              <div
                className="flex items-start"
                style={{ gap: `${COL_GAP}px` }}
              >
                {layers[0]!.map((node) => (
                  <PipelineNode
                    key={node.pipeline_id}
                    ref={setNodeRef(node.pipeline_id)}
                    pipeline={node}
                    bottleneck={node.pipeline_id === bottleneckID}
                  />
                ))}
              </div>
            ) : (
              layers.map((layer, layerIdx) => (
                <Fragment key={`layer-${layerIdx}`}>
                  <div
                    className="flex flex-col"
                    style={{ gap: `${ROW_GAP}px` }}
                  >
                    {layer.map((node) => (
                      <PipelineNode
                        key={node.pipeline_id}
                        ref={setNodeRef(node.pipeline_id)}
                        pipeline={node}
                        bottleneck={node.pipeline_id === bottleneckID}
                      />
                    ))}
                  </div>
                </Fragment>
              ))
            )}
          </div>
        </div>
      </div>
      <VSMMetricsStrip
        nodes={vsm.nodes}
        edges={vsm.edges}
        windowDays={windowDays}
      />
    </div>
  );
}

type EdgePath = {
  key: string;
  // "real" = upstream dep from materials.config (solid line,
  // stage + wait time labels). "implicit" = synthesized between
  // horizontally-adjacent pipelines that have no real edge —
  // dashed line, labeled with the source pipeline's p50 duration
  // so the slowest one pops even without a dependency declared.
  kind: "real" | "implicit";
  // Real-edge labels:
  stage?: string;
  waitSec?: number;
  // Implicit-edge labels:
  sourceP50Sec?: number;
  isOffender?: boolean;
  fromX: number;
  fromY: number;
  toX: number;
  toY: number;
};

// formatWait is intentionally tight: the label sits on a ~40px SVG
// arc and has no room for "2 minutes". Matches the edge-label
// economy of the design reference ("18s", "3m").
function formatWait(sec: number): string {
  if (sec < 1) return "<1s";
  if (sec < 60) return `${Math.round(sec)}s`;
  if (sec < 3600) {
    const m = Math.round(sec / 60);
    return `${m}m`;
  }
  const h = Math.round(sec / 3600);
  return `${h}h`;
}

function nameExists(nodes: VSMNodeT[], name: string): boolean {
  return nodes.some((n) => n.name === name);
}

function firstMetricsWindow(nodes: VSMNodeT[]): number {
  for (const n of nodes) {
    if (n.metrics && n.metrics.window_days > 0) return n.metrics.window_days;
  }
  return 7;
}

// buildLayers groups pipelines by dependency depth. Roots (no
// upstream) are layer 0; each downstream pipeline sits one layer
// deeper than its deepest parent. Same-depth siblings stack
// vertically inside the layer. When there are no edges at all
// every pipeline is depth 0 — the rendering code above lays those
// siblings vertically inside the single layer, which looks right
// for the "parallel tracks from one push" case.
function buildLayers(vsm: ProjectVSM): VSMNodeT[][] {
  const nodeByName = new Map(vsm.nodes.map((n) => [n.name, n]));
  const upstream = new Map<string, string[]>();
  for (const e of vsm.edges) {
    const list = upstream.get(e.to_pipeline) ?? [];
    list.push(e.from_pipeline);
    upstream.set(e.to_pipeline, list);
  }

  const depthMemo = new Map<string, number>();
  const inFlight = new Set<string>();
  function depthOf(name: string): number {
    const cached = depthMemo.get(name);
    if (cached != null) return cached;
    if (inFlight.has(name)) return 0;
    inFlight.add(name);
    const parents = upstream.get(name) ?? [];
    const d =
      parents.length === 0
        ? 0
        : 1 +
          Math.max(
            ...parents
              .filter((p) => nodeByName.has(p))
              .map((p) => depthOf(p)),
          );
    inFlight.delete(name);
    depthMemo.set(name, d);
    return d;
  }

  const byDepth = new Map<number, VSMNodeT[]>();
  for (const node of vsm.nodes) {
    const d = depthOf(node.name);
    const list = byDepth.get(d) ?? [];
    list.push(node);
    byDepth.set(d, list);
  }
  const maxDepth = Math.max(0, ...Array.from(byDepth.keys()));
  const layers: VSMNodeT[][] = [];
  for (let i = 0; i <= maxDepth; i++) {
    const list = byDepth.get(i) ?? [];
    list.sort((a, b) => a.name.localeCompare(b.name));
    layers.push(list);
  }
  return layers;
}

