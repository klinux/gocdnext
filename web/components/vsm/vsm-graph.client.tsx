"use client";

import { useMemo } from "react";
import {
  Background,
  BackgroundVariant,
  Controls,
  type Edge,
  type Node,
  ReactFlow,
  ReactFlowProvider,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";

import type { ProjectVSM, VSMNode as VSMNodeT } from "@/types/api";
import { PipelineNode } from "@/components/vsm/pipeline-node.client";

// Simple grid layout: upstream-roots on the left, each downstream one
// column to the right. Good enough for a fanout of a dozen or two;
// larger graphs would want dagre/elkjs, but that's a follow-up.
const COL_WIDTH = 260;
const ROW_HEIGHT = 140;

const nodeTypes = { pipeline: PipelineNode };

type Props = {
  vsm: ProjectVSM;
};

export function VSMGraph({ vsm }: Props) {
  const { nodes, edges } = useMemo(() => buildGraph(vsm), [vsm]);

  return (
    <ReactFlowProvider>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        fitView
        proOptions={{ hideAttribution: true }}
        nodesDraggable={false}
        nodesConnectable={false}
        panOnDrag
        zoomOnScroll
      >
        <Background variant={BackgroundVariant.Dots} gap={24} size={1} />
        <Controls showInteractive={false} />
      </ReactFlow>
    </ReactFlowProvider>
  );
}

function buildGraph(vsm: ProjectVSM): {
  nodes: Node<{ pipeline: VSMNodeT; slug: string }>[];
  edges: Edge[];
} {
  // Depth = longest upstream chain ending at this pipeline.
  const upstream = new Map<string, string[]>();
  for (const e of vsm.edges) {
    const list = upstream.get(e.to_pipeline) ?? [];
    list.push(e.from_pipeline);
    upstream.set(e.to_pipeline, list);
  }
  const depthMemo = new Map<string, number>();
  function depthOf(name: string, seen: Set<string>): number {
    const cached = depthMemo.get(name);
    if (cached != null) return cached;
    if (seen.has(name)) return 0; // cycle fallback; shouldn't happen
    seen.add(name);
    const parents = upstream.get(name) ?? [];
    const d = parents.length === 0
      ? 0
      : 1 + Math.max(...parents.map((p) => depthOf(p, seen)));
    depthMemo.set(name, d);
    return d;
  }

  // Group by depth so siblings stack vertically in the same column.
  const byDepth = new Map<number, VSMNodeT[]>();
  for (const node of vsm.nodes) {
    const d = depthOf(node.name, new Set());
    const list = byDepth.get(d) ?? [];
    list.push(node);
    byDepth.set(d, list);
  }

  const rfNodes: Node<{ pipeline: VSMNodeT; slug: string }>[] = [];
  for (const [d, list] of byDepth.entries()) {
    list.sort((a, b) => a.name.localeCompare(b.name));
    list.forEach((p, row) => {
      rfNodes.push({
        id: p.pipeline_id,
        type: "pipeline",
        position: { x: d * COL_WIDTH, y: row * ROW_HEIGHT },
        data: { pipeline: p, slug: vsm.project_slug },
        draggable: false,
      });
    });
  }

  const byName = new Map(vsm.nodes.map((n) => [n.name, n.pipeline_id]));
  const rfEdges: Edge[] = vsm.edges
    .map((e, i) => {
      const from = byName.get(e.from_pipeline);
      const to = byName.get(e.to_pipeline);
      if (!from || !to) return null;
      return {
        id: `e${i}-${from}-${to}`,
        source: from,
        target: to,
        label: e.stage,
        animated: false,
        style: { stroke: "var(--muted-foreground)" },
        labelStyle: { fontSize: 10, fill: "var(--muted-foreground)" },
      } as Edge;
    })
    .filter((e): e is Edge => e !== null);

  return { nodes: rfNodes, edges: rfEdges };
}
