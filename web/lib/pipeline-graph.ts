// Shared dependency-grouping for the pipelines listing AND the VSM. Both
// derive "flows" (connected dependency chains) from a node list + edges;
// only the node type differs, so the algorithm is generic over it.

// The minimum an edge must expose to be grouped. PipelineEdge and VSMEdge
// both satisfy this structurally.
export type DepEdge = {
  from_pipeline: string;
  to_pipeline: string;
  stage?: string;
};

// A dependency flow: a connected chain of >=2 nodes in topological order
// (upstream first), plus the edges internal to the chain.
export type FlowGroup<T, E extends DepEdge = DepEdge> = {
  // endpoint path for the header, e.g. "build → deploy".
  path: string;
  nodes: T[];
  edges: E[];
};

export type DepGrouping<T, E extends DepEdge = DepEdge> = {
  flows: FlowGroup<T, E>[];
  independent: T[];
};

// groupByDependency partitions nodes into dependency flows (connected
// components with >=1 edge, topologically ordered) and "independent" nodes
// (no edges). Pure + deterministic — input order is the tie breaker so the
// layout never jumps between renders. `nameOf` maps a node to the name the
// edges reference.
export function groupByDependency<T, E extends DepEdge>(
  nodes: T[],
  edges: E[],
  nameOf: (node: T) => string,
): DepGrouping<T, E> {
  const byName = new Map(nodes.map((n) => [nameOf(n), n]));

  // Drop stale/self edges so a dangling ref can't fabricate a phantom node.
  const valid = edges.filter(
    (e) =>
      e.from_pipeline !== e.to_pipeline &&
      byName.has(e.from_pipeline) &&
      byName.has(e.to_pipeline),
  );

  // Undirected adjacency → connected components; directed in/out → topo order.
  const adj = new Map<string, Set<string>>();
  const touched = new Set<string>();
  const link = (a: string, b: string) => {
    if (!adj.has(a)) adj.set(a, new Set());
    adj.get(a)?.add(b);
  };
  for (const e of valid) {
    touched.add(e.from_pipeline);
    touched.add(e.to_pipeline);
    link(e.from_pipeline, e.to_pipeline);
    link(e.to_pipeline, e.from_pipeline);
  }

  const seen = new Set<string>();
  const flows: FlowGroup<T, E>[] = [];
  // Iterate in the given order so component discovery is deterministic.
  for (const node of nodes) {
    const name = nameOf(node);
    if (!touched.has(name) || seen.has(name)) continue;
    const comp: string[] = [];
    const queue = [name];
    seen.add(name);
    while (queue.length > 0) {
      const n = queue.shift() as string;
      comp.push(n);
      for (const m of adj.get(n) ?? []) {
        if (!seen.has(m)) {
          seen.add(m);
          queue.push(m);
        }
      }
    }
    if (comp.length < 2) continue; // a touched node always has >=1 neighbour
    const compSet = new Set(comp);
    const compEdges = valid.filter(
      (e) => compSet.has(e.from_pipeline) && compSet.has(e.to_pipeline),
    );
    const ordered = topoOrder(comp, compEdges);
    flows.push({
      path: `${ordered[0]} → ${ordered[ordered.length - 1]}`,
      nodes: ordered.map((n) => byName.get(n) as T),
      edges: compEdges,
    });
  }

  const independent = nodes.filter((n) => !touched.has(nameOf(n)));
  return { flows, independent };
}

// topoOrder sorts a component's nodes upstream→downstream (Kahn's
// algorithm). Ties break by `nodes` order for stability. A cycle (which
// pipelines shouldn't form) degrades gracefully: leftover nodes are
// appended in input order rather than dropped.
function topoOrder(nodes: string[], edges: DepEdge[]): string[] {
  const rank = new Map(nodes.map((n, i) => [n, i]));
  const indeg = new Map(nodes.map((n) => [n, 0]));
  const out = new Map<string, string[]>(nodes.map((n) => [n, []]));
  for (const e of edges) {
    out.get(e.from_pipeline)?.push(e.to_pipeline);
    indeg.set(e.to_pipeline, (indeg.get(e.to_pipeline) ?? 0) + 1);
  }

  const ready = nodes.filter((n) => (indeg.get(n) ?? 0) === 0);
  const done = new Set<string>();
  const result: string[] = [];
  while (ready.length > 0) {
    ready.sort((a, b) => (rank.get(a) ?? 0) - (rank.get(b) ?? 0));
    const n = ready.shift() as string;
    if (done.has(n)) continue;
    done.add(n);
    result.push(n);
    for (const m of out.get(n) ?? []) {
      indeg.set(m, (indeg.get(m) ?? 0) - 1);
      if ((indeg.get(m) ?? 0) === 0 && !done.has(m)) ready.push(m);
    }
  }
  for (const n of nodes) if (!done.has(n)) result.push(n); // cycle fallback
  return result;
}
