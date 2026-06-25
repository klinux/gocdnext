import type { ProjectVSM, VSMEdge, VSMNode } from "@/types/api";
import { groupByDependency } from "@/lib/pipeline-graph";

// One pipeline within a stream, with the value-stream metrics the VSM
// renders. Times are seconds; caRate is 0..1 (null when there's no run
// data in the window).
export type StreamStep = {
  node: VSMNode;
  status?: string;
  processSec: number;
  // Wait before this step — the handoff/queue time on the incoming edge.
  waitInSec: number;
  // ".stage" of the incoming edge (the artifact handed off), null for the
  // first step.
  artifactIn: string | null;
  // Lead accumulated from commit through this step finishing.
  cumulativeLeadSec: number;
  caRate: number | null;
  throughputPerDay: number | null;
  bottleneck: boolean;
};

// A value stream: one connected dependency chain, with path-level
// aggregates the summary tiles render.
export type Stream = {
  path: string;
  steps: StreamStep[];
  leadTotalSec: number;
  processTotalSec: number;
  // process ÷ lead — how much of the lead time is real work vs waiting.
  flowEfficiency: number | null;
  // product of the chain's yields — one step at 0% zeroes the stream.
  rolledCA: number | null;
  bottleneckName: string | null;
};

// Project-wide DORA rollup across every pipeline (the panel the old VSM
// metrics strip carried). Averages are weighted by runs so a chatty
// pipeline doesn't get drowned out by an idle one.
export type ProjectRollup = {
  leadP50AvgSec: number;
  processP50AvgSec: number;
  successAvg: number | null;
  pipelineCount: number;
  runsTotal: number;
  windowDays: number;
  // The weakest chain's yield — the binding constraint on delivery.
  worstRolledCA: number | null;
};

export type VSMStreams = {
  streams: Stream[];
  // Pipelines off the path to production (no dependency edges).
  outside: VSMNode[];
  rollup: ProjectRollup;
};

// buildVSMStreams turns the raw ProjectVSM into value streams: it groups
// nodes by dependency chain, then computes per-step + path-level metrics.
// Pure — all values derive from the nodes' metrics + the edges' wait times.
export function buildVSMStreams(vsm: ProjectVSM): VSMStreams {
  const { flows, independent } = groupByDependency(
    vsm.nodes,
    vsm.edges,
    (n) => n.name,
  );

  const streams = flows.map((flow) => buildStream(flow.path, flow.nodes, flow.edges));
  return {
    streams,
    outside: independent,
    rollup: buildProjectRollup(vsm.nodes, streams),
  };
}

// buildProjectRollup aggregates every pipeline into a DORA-style summary:
// runs-weighted lead/process/success averages, the pipeline count, and the
// worst chain's rolled yield.
function buildProjectRollup(nodes: VSMNode[], streams: Stream[]): ProjectRollup {
  let wLead = 0;
  let wProc = 0;
  let wSucc = 0;
  let runs = 0;
  let windowDays = 7;
  for (const n of nodes) {
    const m = n.metrics;
    if (!m || m.runs_considered <= 0) continue;
    runs += m.runs_considered;
    wLead += m.lead_time_p50_seconds * m.runs_considered;
    wProc += m.process_time_p50_seconds * m.runs_considered;
    wSucc += m.success_rate * m.runs_considered;
    if (m.window_days > 0) windowDays = m.window_days;
  }
  const rolled = streams
    .map((s) => s.rolledCA)
    .filter((r): r is number => r != null);
  return {
    leadP50AvgSec: runs > 0 ? wLead / runs : 0,
    processP50AvgSec: runs > 0 ? wProc / runs : 0,
    successAvg: runs > 0 ? wSucc / runs : null,
    pipelineCount: nodes.length,
    runsTotal: runs,
    windowDays,
    worstRolledCA: rolled.length > 0 ? Math.min(...rolled) : null,
  };
}

function buildStream(path: string, nodes: VSMNode[], edges: VSMEdge[]): Stream {
  const waitInto = new Map<string, number>();
  const stageInto = new Map<string, string>();
  for (const e of edges) {
    waitInto.set(e.to_pipeline, e.wait_time_p50_seconds ?? 0);
    if (e.stage) stageInto.set(e.to_pipeline, e.stage);
  }

  let cumulative = 0;
  let processTotal = 0;
  const steps: StreamStep[] = nodes.map((node, i) => {
    const m = node.metrics;
    const processSec = m?.process_time_p50_seconds ?? 0;
    const waitInSec = i === 0 ? 0 : (waitInto.get(node.name) ?? 0);
    cumulative += waitInSec + processSec;
    processTotal += processSec;
    const stage = stageInto.get(node.name);
    return {
      node,
      status: node.latest_run?.status,
      processSec,
      waitInSec,
      artifactIn: i === 0 ? null : stage ? `.${stage}` : null,
      cumulativeLeadSec: cumulative,
      caRate: caRate(node),
      throughputPerDay: throughput(node),
      bottleneck: false,
    };
  });

  const bottleneckName = pickBottleneck(steps);
  for (const s of steps) s.bottleneck = s.node.name === bottleneckName;

  const leadTotalSec = cumulative;
  const flowEfficiency = leadTotalSec > 0 ? processTotal / leadTotalSec : null;
  const rolledCA = rolled(steps);

  return {
    path,
    steps,
    leadTotalSec,
    processTotalSec: processTotal,
    flowEfficiency,
    rolledCA,
    bottleneckName,
  };
}

// caRate is the change-approval yield — only meaningful with runs in the
// window, so an unrun/young pipeline reports null (not a misleading 0%).
function caRate(node: VSMNode): number | null {
  const m = node.metrics;
  if (!m || m.runs_considered <= 0) return null;
  return m.success_rate;
}

function throughput(node: VSMNode): number | null {
  const m = node.metrics;
  if (!m || m.window_days <= 0) return null;
  return m.runs_considered / m.window_days;
}

// pickBottleneck flags the step dragging the stream down: the lowest yield
// (a low C/A poisons the rolled number), tie-broken by the longest process
// time. Falls back to the slowest step when no step has yield data.
function pickBottleneck(steps: StreamStep[]): string | null {
  const withCA = steps.filter((s) => s.caRate != null);
  if (withCA.length > 0) {
    let worst = withCA[0]!;
    for (const s of withCA) {
      if (
        s.caRate! < worst.caRate! ||
        (s.caRate === worst.caRate && s.processSec > worst.processSec)
      ) {
        worst = s;
      }
    }
    return worst.node.name;
  }
  let slowest: StreamStep | null = null;
  for (const s of steps) {
    if (s.processSec > 0 && (!slowest || s.processSec > slowest.processSec)) {
      slowest = s;
    }
  }
  return slowest?.node.name ?? null;
}

// rolled multiplies the chain's yields. Steps without data are skipped (a
// young pipeline shouldn't zero the stream); null when no step has data.
function rolled(steps: StreamStep[]): number | null {
  const rates = steps.map((s) => s.caRate).filter((r): r is number => r != null);
  if (rates.length === 0) return null;
  return rates.reduce((acc, r) => acc * r, 1);
}
