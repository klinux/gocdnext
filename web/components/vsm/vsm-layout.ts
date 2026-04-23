import type { ProjectVSM, VSMNode as VSMNodeT } from "@/types/api";

// buildLayers groups pipelines by dependency depth then orders
// siblings within each layer so parent→child edges cross as
// little as possible.
//
// Depth pass: roots (no upstream) are layer 0; each downstream
// pipeline sits one layer deeper than its deepest parent. Cycles
// (shouldn't happen, but defend) bail to depth 0.
//
// Ordering pass (barycenter heuristic): walking layers left to
// right, each node's row index is pinned by the average index of
// its already-placed parents. A node with no parents in the
// previous layer falls back to alphabetical. Right-to-left pass
// then tightens the parent ordering against child positions —
// two sweeps is enough for small pipeline DAGs and matches what
// dagre / graphviz do in their `order` step. Without this, a
// root like `ci-server` sat alphabetically between `ci-agent`
// and `ci-cli` while its only child `ci-web` was alone at the
// top of the next column, so the arrow had to jump three rows
// and cross unrelated cards.
export function buildLayers(vsm: ProjectVSM): VSMNodeT[][] {
  const nodeByName = new Map(vsm.nodes.map((n) => [n.name, n]));
  const upstream = new Map<string, string[]>();
  const downstream = new Map<string, string[]>();
  for (const e of vsm.edges) {
    const ups = upstream.get(e.to_pipeline) ?? [];
    ups.push(e.from_pipeline);
    upstream.set(e.to_pipeline, ups);
    const downs = downstream.get(e.from_pipeline) ?? [];
    downs.push(e.to_pipeline);
    downstream.set(e.from_pipeline, downs);
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

  // Barycenter ordering. Run a down-sweep followed by an up-sweep
  // — two iterations land close enough to the minimum-crossing
  // order for pipeline counts we actually see (< ~50 per project).
  const indexIn = (layer: VSMNodeT[], name: string) =>
    layer.findIndex((n) => n.name === name);

  const reorder = (
    layer: VSMNodeT[],
    neighbors: Map<string, string[]>,
    ref: VSMNodeT[],
  ): VSMNodeT[] => {
    return [...layer].sort((a, b) => {
      const aNeighbors = (neighbors.get(a.name) ?? [])
        .map((n) => indexIn(ref, n))
        .filter((i) => i >= 0);
      const bNeighbors = (neighbors.get(b.name) ?? [])
        .map((n) => indexIn(ref, n))
        .filter((i) => i >= 0);
      const aScore =
        aNeighbors.length === 0
          ? Number.POSITIVE_INFINITY
          : aNeighbors.reduce((s, x) => s + x, 0) / aNeighbors.length;
      const bScore =
        bNeighbors.length === 0
          ? Number.POSITIVE_INFINITY
          : bNeighbors.reduce((s, x) => s + x, 0) / bNeighbors.length;
      if (aScore !== bScore) return aScore - bScore;
      return a.name.localeCompare(b.name);
    });
  };

  // Down-sweep: order each layer by its parents' indices in the
  // previous (already-ordered) layer.
  for (let i = 1; i < layers.length; i++) {
    layers[i] = reorder(layers[i]!, upstream, layers[i - 1]!);
  }
  // Up-sweep: tighten parents against child positions. Gives roots
  // with a single child a chance to sit right next to that child.
  for (let i = layers.length - 2; i >= 0; i--) {
    layers[i] = reorder(layers[i]!, downstream, layers[i + 1]!);
  }
  return layers;
}
