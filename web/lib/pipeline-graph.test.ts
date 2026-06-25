import { describe, expect, it } from "vitest";

import { groupByDependency } from "./pipeline-graph";
import type { PipelineEdge, PipelineSummary } from "@/types/api";

// Minimal fixture — grouping only reads `name`.
function p(name: string): PipelineSummary {
  return { id: name, name, definition_version: 1, updated_at: "" };
}

function edge(from: string, to: string, stage?: string): PipelineEdge {
  return { from_pipeline: from, to_pipeline: to, stage };
}

// The listing always groups PipelineSummary by name; thin wrapper keeps
// the assertions terse.
function group(nodes: PipelineSummary[], edges: PipelineEdge[]) {
  return groupByDependency(nodes, edges, (x) => x.name);
}

describe("groupByDependency", () => {
  it("matches the handoff example: two flows + two independents", () => {
    const pipelines = [
      p("build"),
      p("deploy"),
      p("gravitee-stage"),
      p("gravitee-prod"),
      p("build-pr"),
      p("security"),
    ];
    const edges = [
      edge("build", "deploy", "image"),
      edge("gravitee-stage", "gravitee-prod", "publish"),
    ];

    const { flows, independent } = group(pipelines, edges);

    expect(flows.map((f) => f.path)).toEqual([
      "build → deploy",
      "gravitee-stage → gravitee-prod",
    ]);
    expect(flows[0]!.nodes.map((x) => x.name)).toEqual(["build", "deploy"]);
    expect(independent.map((x) => x.name)).toEqual(["build-pr", "security"]);
  });

  it("orders a chain upstream → downstream regardless of input order", () => {
    // deploy listed before its upstream build.
    const { flows } = group(
      [p("deploy"), p("build")],
      [edge("build", "deploy")],
    );
    expect(flows[0]!.nodes.map((x) => x.name)).toEqual(["build", "deploy"]);
    expect(flows[0]!.path).toBe("build → deploy");
  });

  it("orders a diamond (a→b, a→c, b→d, c→d) with a first and d last", () => {
    const { flows } = group(
      [p("d"), p("c"), p("b"), p("a")],
      [edge("a", "b"), edge("a", "c"), edge("b", "d"), edge("c", "d")],
    );
    const order = flows[0]!.nodes.map((x) => x.name);
    expect(order[0]).toBe("a");
    expect(order[order.length - 1]).toBe("d");
    expect(order).toHaveLength(4);
  });

  it("treats pipelines with no edges as independent", () => {
    const { flows, independent } = group([p("a"), p("b")], []);
    expect(flows).toHaveLength(0);
    expect(independent.map((x) => x.name)).toEqual(["a", "b"]);
  });

  it("ignores edges that reference a missing pipeline", () => {
    const { flows, independent } = group(
      [p("build")],
      [edge("build", "ghost"), edge("build", "build")],
    );
    expect(flows).toHaveLength(0);
    expect(independent.map((x) => x.name)).toEqual(["build"]);
  });

  it("does not drop nodes when the graph has a cycle", () => {
    const { flows } = group(
      [p("a"), p("b"), p("c")],
      [edge("a", "b"), edge("b", "c"), edge("c", "a")],
    );
    expect(flows[0]!.nodes.map((x) => x.name).sort()).toEqual([
      "a",
      "b",
      "c",
    ]);
  });
});
