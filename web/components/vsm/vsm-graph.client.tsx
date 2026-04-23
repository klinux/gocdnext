"use client";

import { Fragment, useLayoutEffect, useMemo, useRef, useState } from "react";

import type { ProjectVSM, VSMNode as VSMNodeT } from "@/types/api";
import { PipelineNode } from "@/components/vsm/pipeline-node.client";
import { buildLayers } from "@/components/vsm/vsm-layout";
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
  // VSM renders all pipelines in a single horizontal row. The flat
  // order comes from a DFS that emits each root immediately
  // followed by its transitively-reachable downstreams: parent and
  // child always end up adjacent, so the real-upstream arrow
  // (ci-server → ci-web) stays a short rightward hop instead of
  // snaking past every unrelated root. A plain layers.flat()
  // would've dumped all of layer 0 first then layer 1, leaving
  // ci-web stranded at the end of the row.
  //
  // Roots come from layer[0] in barycenter order (alphabetical when
  // no sibling has a downstream; otherwise parents-with-children
  // float toward the position their child occupies). Any node the
  // DFS doesn't reach — shouldn't happen for a well-formed DAG but
  // we defend against cycles that buildLayers already handled — is
  // appended in layer/alphabetical order at the tail.
  const orderedNodes = useMemo(() => {
    const byName = new Map(vsm.nodes.map((n) => [n.name, n]));
    const childrenOf = new Map<string, string[]>();
    for (const e of vsm.edges) {
      const list = childrenOf.get(e.from_pipeline) ?? [];
      list.push(e.to_pipeline);
      childrenOf.set(e.from_pipeline, list);
    }
    const seen = new Set<string>();
    const out: VSMNodeT[] = [];
    const visit = (name: string) => {
      if (seen.has(name)) return;
      seen.add(name);
      const n = byName.get(name);
      if (n) out.push(n);
      for (const child of childrenOf.get(name) ?? []) visit(child);
    };
    if (layers.length > 0) {
      for (const root of layers[0]!) visit(root.name);
    }
    // Pick up anything the DFS missed (cycles, disconnected
    // components). Last-resort tail, alphabetical within layer.
    for (const layer of layers) {
      for (const n of layer) {
        if (!seen.has(n.name)) {
          seen.add(n.name);
          out.push(n);
        }
      }
    }
    return out;
  }, [layers, vsm.nodes, vsm.edges]);

  const implicitEdges = useMemo(() => {
    if (orderedNodes.length < 2) return [];
    // Skip pairs that already have a real upstream edge — drawing
    // both a dashed-sequence arrow AND the solid-dependency arrow
    // between the same two cards doubles the visual noise without
    // extra info.
    const realPairs = new Set(
      renderableEdges.map((e) => `${e.from_pipeline}->${e.to_pipeline}`),
    );
    const out: { fromID: string; toID: string; sourcePipelineID: string }[] = [];
    for (let i = 0; i < orderedNodes.length - 1; i++) {
      const from = orderedNodes[i]!;
      const to = orderedNodes[i + 1]!;
      if (realPairs.has(`${from.name}->${to.name}`)) continue;
      out.push({
        fromID: from.pipeline_id,
        toID: to.pipeline_id,
        sourcePipelineID: from.pipeline_id,
      });
    }
    return out;
  }, [orderedNodes, renderableEdges]);

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
      // anchorFor picks exit/entry points on each card that sit
      // on the axis where the two cards are most separated. In
      // the row-per-layer layout parent→child goes bottom→top
      // (vertical); within a single row an implicit edge between
      // siblings goes right→left (horizontal). Picking the axis
      // dynamically lets the same routing code serve both.
      const anchorFor = (f: DOMRect, t: DOMRect) => {
        const dxCenter = t.left + t.width / 2 - (f.left + f.width / 2);
        const dyCenter = t.top + t.height / 2 - (f.top + f.height / 2);
        if (Math.abs(dyCenter) > Math.abs(dxCenter)) {
          // Vertical edge — anchor on top/bottom edges.
          const fromY = dyCenter > 0 ? f.bottom : f.top;
          const toY = dyCenter > 0 ? t.top : t.bottom;
          return {
            fromX: f.left + f.width / 2 - cRect.left + container.scrollLeft,
            fromY: fromY - cRect.top + container.scrollTop,
            toX: t.left + t.width / 2 - cRect.left + container.scrollLeft,
            toY: toY - cRect.top + container.scrollTop,
          };
        }
        // Horizontal edge — anchor on left/right edges.
        const fromX = dxCenter > 0 ? f.right : f.left;
        const toX = dxCenter > 0 ? t.left : t.right;
        return {
          fromX: fromX - cRect.left + container.scrollLeft,
          fromY: f.top + f.height / 2 - cRect.top + container.scrollTop,
          toX: toX - cRect.left + container.scrollLeft,
          toY: t.top + t.height / 2 - cRect.top + container.scrollTop,
        };
      };

      for (const e of renderableEdges) {
        const fromID = byName.get(e.from_pipeline);
        const toID = byName.get(e.to_pipeline);
        if (!fromID || !toID) continue;
        const from = nodeRefs.current.get(fromID);
        const to = nodeRefs.current.get(toID);
        if (!from || !to) continue;
        next.push({
          key: `real-${e.from_pipeline}->${e.to_pipeline}`,
          kind: "real",
          stage: e.stage,
          waitSec: e.wait_time_p50_seconds,
          ...anchorFor(from.getBoundingClientRect(), to.getBoundingClientRect()),
        });
      }
      for (const ie of implicitEdges) {
        const from = nodeRefs.current.get(ie.fromID);
        const to = nodeRefs.current.get(ie.toID);
        if (!from || !to) continue;
        next.push({
          key: `implicit-${ie.fromID}->${ie.toID}`,
          kind: "implicit",
          sourceP50Sec: p50ByID.get(ie.sourcePipelineID),
          isOffender: ie.sourcePipelineID === slowestPipelineID,
          ...anchorFor(from.getBoundingClientRect(), to.getBoundingClientRect()),
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
              // Choose the bezier control-point axis so the curve
              // bulges along the dominant travel direction.
              // Row-per-layer layouts make most edges primarily
              // vertical (parent in row N, child in row N+1); a
              // pure-X offset would draw a horizontal S-curve
              // between two vertically-aligned points, which reads
              // nonsensical. Pick whichever delta is larger.
              const dxRaw = p.toX - p.fromX;
              const dyRaw = p.toY - p.fromY;
              const vertical = Math.abs(dyRaw) > Math.abs(dxRaw);
              const offset = Math.max(40, Math.abs(vertical ? dyRaw : dxRaw) / 2);
              const c1x = vertical ? p.fromX : p.fromX + Math.sign(dxRaw || 1) * offset;
              const c1y = vertical ? p.fromY + Math.sign(dyRaw || 1) * offset : p.fromY;
              const c2x = vertical ? p.toX : p.toX - Math.sign(dxRaw || 1) * offset;
              const c2y = vertical ? p.toY - Math.sign(dyRaw || 1) * offset : p.toY;
              const midX = (p.fromX + p.toX) / 2;
              const midY = (p.fromY + p.toY) / 2;
              const isImplicit = p.kind === "implicit";
              return (
                <g key={p.key}>
                  <path
                    d={`M ${p.fromX} ${p.fromY} C ${c1x} ${c1y}, ${c2x} ${c2y}, ${p.toX} ${p.toY}`}
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

        {/* Single horizontal row. Barycenter ordering inside
            buildLayers already put parent/child pairs side-by-side
            (ci-server next to its downstream ci-web) and left the
            rest alphabetical. Implicit dashed edges connect every
            adjacent pair that doesn't have a real upstream edge
            so the "this came before that" sequence stays readable
            even when pipelines don't depend on each other
            formally. Overflow scrolls horizontally when the DAG
            is wider than the viewport. */}
        <div className="relative flex min-w-full justify-center">
          <div
            className="inline-flex items-start"
            style={{ gap: `${COL_GAP}px` }}
          >
            {orderedNodes.map((node) => (
              <PipelineNode
                key={node.pipeline_id}
                ref={setNodeRef(node.pipeline_id)}
                pipeline={node}
                bottleneck={node.pipeline_id === bottleneckID}
              />
            ))}
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


