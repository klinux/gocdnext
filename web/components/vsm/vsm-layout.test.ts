import { describe, expect, it } from "vitest";

import type { ProjectVSM, VSMEdge, VSMNode } from "@/types/api";
import { buildLayers } from "@/components/vsm/vsm-layout";

// Stub nodes / edges with only the fields buildLayers touches.
// Cast through `unknown` so the compiler lets us skip the unused
// ones (pipeline_id, metrics, etc.) — if buildLayers ever reads a
// new field, the test suite fails loud instead of silently.
const node = (name: string): VSMNode =>
  ({ id: name, name } as unknown as VSMNode);

const edge = (from: string, to: string, stage = "test"): VSMEdge =>
  ({ from_pipeline: from, to_pipeline: to, stage } as unknown as VSMEdge);

const vsm = (nodes: VSMNode[], edges: VSMEdge[]): ProjectVSM =>
  ({ nodes, edges } as unknown as ProjectVSM);

const names = (layer: VSMNode[]) => layer.map((n) => n.name);

describe("buildLayers", () => {
  it("places dependency parents adjacent to their only child", () => {
    // The motivating case: ci-web depends on ci-server.test;
    // ci-agent / ci-cli / lint are independent roots. Without the
    // barycenter up-sweep ci-server sat alphabetically in the
    // middle of column 0 while ci-web was alone at the top of
    // column 1, so the arrow zigzagged across two unrelated cards.
    // After the up-sweep ci-server should pop to the top of column
    // 0 — horizontal arrow, zero crossings.
    const g = vsm(
      [
        node("ci-agent"),
        node("ci-cli"),
        node("ci-server"),
        node("ci-web"),
        node("lint"),
      ],
      [edge("ci-server", "ci-web")],
    );

    const layers = buildLayers(g);
    expect(layers).toHaveLength(2);
    expect(names(layers[0]!)).toEqual(["ci-server", "ci-agent", "ci-cli", "lint"]);
    expect(names(layers[1]!)).toEqual(["ci-web"]);
  });

  it("keeps pure-alphabetical order when no edges exist", () => {
    // Without any upstream edges every pipeline is depth 0 and the
    // ordering logic should fall through to the alphabetical tie-
    // break — no surprises for projects that haven't opted into
    // upstream triggers yet.
    const g = vsm([node("zeta"), node("alpha"), node("beta")], []);
    const layers = buildLayers(g);
    expect(layers).toHaveLength(1);
    expect(names(layers[0]!)).toEqual(["alpha", "beta", "zeta"]);
  });

  it("depths multi-level chains correctly", () => {
    // Deeper DAGs (a → b → c) should spread across three layers.
    const g = vsm(
      [node("a"), node("b"), node("c")],
      [edge("a", "b"), edge("b", "c")],
    );
    const layers = buildLayers(g);
    expect(names(layers[0]!)).toEqual(["a"]);
    expect(names(layers[1]!)).toEqual(["b"]);
    expect(names(layers[2]!)).toEqual(["c"]);
  });

  it("orders siblings in a child layer by parent position", () => {
    // Two roots each feeding one child — the children should line
    // up with their parents (no cross). If we sorted children
    // alphabetically we'd swap them and create a cross.
    const g = vsm(
      [
        node("parent-a"),
        node("parent-b"),
        node("child-of-b"), // alphabetical would place this first
        node("child-of-a"),
      ],
      [edge("parent-a", "child-of-a"), edge("parent-b", "child-of-b")],
    );
    const layers = buildLayers(g);
    expect(layers).toHaveLength(2);
    // Parents alphabetical by default; children ordered by their
    // parent's index in layer 0.
    expect(names(layers[0]!)).toEqual(["parent-a", "parent-b"]);
    expect(names(layers[1]!)).toEqual(["child-of-a", "child-of-b"]);
  });

  it("handles cycles defensively (no infinite recursion)", () => {
    // Cycles shouldn't happen in a well-formed pipeline DAG, but
    // the depth pass has an in-flight guard that bails to 0. The
    // test just confirms we return in finite time and produce
    // something non-empty rather than hanging or throwing.
    const g = vsm(
      [node("a"), node("b")],
      [edge("a", "b"), edge("b", "a")],
    );
    const layers = buildLayers(g);
    const flat = layers.flat().map((n) => n.name);
    expect(flat.sort()).toEqual(["a", "b"]);
  });
});
