import { describe, expect, it } from "vitest";

import { buildVSMStreams } from "./vsm-stream";
import type {
  PipelineMetrics,
  ProjectVSM,
  RunSummary,
  VSMEdge,
  VSMNode,
} from "@/types/api";

function metrics(over: Partial<PipelineMetrics>): PipelineMetrics {
  return {
    window_days: 7,
    runs_considered: 7,
    success_rate: 1,
    lead_time_p50_seconds: 0,
    process_time_p50_seconds: 0,
    ...over,
  };
}

function node(name: string, opts: { status?: string; metrics?: PipelineMetrics }): VSMNode {
  return {
    pipeline_id: name,
    name,
    definition_version: 1,
    latest_run: opts.status ? ({ status: opts.status } as RunSummary) : undefined,
    metrics: opts.metrics,
  };
}

function edge(from: string, to: string, stage: string, wait: number): VSMEdge {
  return {
    from_pipeline: from,
    to_pipeline: to,
    stage,
    wait_time_p50_seconds: wait,
  };
}

function vsm(nodes: VSMNode[], edges: VSMEdge[]): ProjectVSM {
  return {
    project_id: "p",
    project_slug: "p",
    project_name: "p",
    nodes,
    edges,
    generated_at: "",
  };
}

describe("buildVSMStreams", () => {
  const built = buildVSMStreams(
    vsm(
      [
        node("build", {
          status: "success",
          metrics: metrics({ process_time_p50_seconds: 280, success_rate: 0.58 }),
        }),
        node("deploy", {
          status: "failed",
          metrics: metrics({
            process_time_p50_seconds: 225,
            success_rate: 0,
            runs_considered: 14,
          }),
        }),
        node("build-pr", { metrics: metrics({ success_rate: 0.56 }) }),
        node("security", { metrics: metrics({ success_rate: 0.27 }) }),
      ],
      [edge("build", "deploy", "image", 1)],
    ),
  );

  it("splits the path-to-prod stream from the independent pipelines", () => {
    expect(built.streams).toHaveLength(1);
    expect(built.streams[0]!.path).toBe("build → deploy");
    expect(built.outside.map((n) => n.name)).toEqual(["build-pr", "security"]);
  });

  it("accumulates lead time across waits + processes", () => {
    const [b, d] = built.streams[0]!.steps;
    expect(b!.cumulativeLeadSec).toBe(280); // no wait into the first step
    expect(d!.waitInSec).toBe(1);
    expect(d!.artifactIn).toBe(".image");
    expect(d!.cumulativeLeadSec).toBe(280 + 1 + 225);
  });

  it("computes path aggregates incl. rolled C/A (one 0% zeroes it)", () => {
    const s = built.streams[0]!;
    expect(s.leadTotalSec).toBe(506);
    expect(s.processTotalSec).toBe(505);
    expect(s.flowEfficiency).toBeCloseTo(505 / 506, 5);
    expect(s.rolledCA).toBe(0); // 0.58 * 0
  });

  it("flags the lowest-yield step as the bottleneck", () => {
    const s = built.streams[0]!;
    expect(s.bottleneckName).toBe("deploy");
    expect(s.steps.find((x) => x.node.name === "deploy")!.bottleneck).toBe(true);
    expect(s.steps.find((x) => x.node.name === "build")!.bottleneck).toBe(false);
  });

  it("derives throughput per day from runs / window", () => {
    const [b, d] = built.streams[0]!.steps;
    expect(b!.throughputPerDay).toBe(1); // 7 runs / 7d
    expect(d!.throughputPerDay).toBe(2); // 14 runs / 7d
  });

  it("rolls up project-wide DORA averages weighted by runs", () => {
    const r = built.rollup;
    expect(r.pipelineCount).toBe(4); // streams + outside
    expect(r.runsTotal).toBe(35); // 7 + 14 + 7 + 7
    // (0.58·7 + 0·14 + 0.56·7 + 0.27·7) / 35
    expect(r.successAvg).toBeCloseTo(0.282, 2);
    expect(r.worstRolledCA).toBe(0); // the build→deploy chain is 0%
  });

  it("reports null C/A for a pipeline with no runs in the window", () => {
    const { streams } = buildVSMStreams(
      vsm(
        [
          node("a", { metrics: metrics({ process_time_p50_seconds: 10 }) }),
          node("b", { metrics: metrics({ runs_considered: 0 }) }),
        ],
        [edge("a", "b", "out", 0)],
      ),
    );
    const b = streams[0]!.steps.find((s) => s.node.name === "b")!;
    expect(b.caRate).toBeNull();
    expect(b.throughputPerDay).toBe(0); // 0 runs / 7d
  });
});
