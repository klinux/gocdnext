"use client";

import { Fragment, useMemo, useRef } from "react";
import { AlertTriangle } from "lucide-react";

import { cn } from "@/lib/utils";
import { PipelineCard } from "@/components/pipelines/pipeline-card";
import { statusTone, type StatusTone } from "@/lib/status";
import type { PipelineEdge, PipelineSummary, RunSummary } from "@/types/api";

type Props = {
  projectSlug: string;
  pipelines: PipelineSummary[];
  edges: PipelineEdge[];
  runs: RunSummary[];
};

// PipelineFlow lays the project's pipelines out as cells in a grid.
// A cell is either a single pipeline card OR a vertical chain when
// a run of pipelines forms a strict 1-to-1 dependency (downstream
// has exactly one upstream and that upstream has exactly one
// downstream). Chained pipelines stack with a thin dashed line on
// the left margin connecting them — the timeline-style read of
// "this triggers the next" without the SVG-arrow lasso the layered
// DAG used to draw across the page.
//
// Fan-in / fan-out / cross-cell dependencies stay legible via the
// upstream pill in each downstream card's header. Drawing arrows
// across grid cells looked tangled enough that the user asked for
// them gone — the chain stack covers the common case (linear
// upstream chains), pills cover the rest.
export function PipelineFlow({ projectSlug, pipelines, edges, runs }: Props) {
  const cells = useMemo(
    () => buildCells(pipelines, edges),
    [pipelines, edges],
  );

  const containerRef = useRef<HTMLDivElement>(null);
  const cardRefs = useRef(new Map<string, HTMLElement>());

  if (pipelines.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No pipelines yet. Push a YAML to the repo&apos;s config folder or run{" "}
        <code className="font-mono">gocdnext apply</code>.
      </p>
    );
  }

  const alerts = pipelines.filter(isAlerting);

  // Sort cells: alert weight first (failing → degraded → healthy),
  // then alphabetical. For chain cells we use the worst weight
  // anywhere in the chain so a partly-failing chain bubbles up.
  const sortedCells = [...cells].sort((a, b) => {
    const wa = cellWeight(a);
    const wb = cellWeight(b);
    if (wa !== wb) return wb - wa;
    return cellName(a).localeCompare(cellName(b));
  });

  const setCardRef = (name: string) => (el: HTMLElement | null) => {
    if (el) cardRefs.current.set(name, el);
    else cardRefs.current.delete(name);
  };

  const focusCard = (name: string) => {
    const el = cardRefs.current.get(name);
    if (el) {
      el.scrollIntoView({ behavior: "smooth", block: "center" });
      el.classList.add("ring-2", "ring-amber-500/60");
      window.setTimeout(
        () => el.classList.remove("ring-2", "ring-amber-500/60"),
        1500,
      );
    }
  };

  return (
    <div ref={containerRef} className="relative space-y-4">
      {alerts.length > 0 ? (
        <div className="flex flex-wrap items-center gap-2 rounded-md border border-amber-500/40 bg-amber-500/5 px-3 py-2 text-[12px]">
          <AlertTriangle className="size-4 shrink-0 text-amber-600 dark:text-amber-400" aria-hidden />
          <span className="font-medium text-amber-700 dark:text-amber-400">
            {alerts.length === 1
              ? "1 pipeline needs attention:"
              : `${alerts.length} pipelines need attention:`}
          </span>
          <div className="flex flex-wrap items-center gap-1.5">
            {alerts.map((p) => (
              <button
                key={p.id}
                type="button"
                onClick={() => focusCard(p.name)}
                className="inline-flex items-center gap-1.5 rounded-full border border-amber-500/40 bg-card px-2 py-0.5 font-mono text-[11px] hover:bg-amber-500/10"
                title={alertReason(p)}
              >
                <span
                  className={cn(
                    "size-1.5 rounded-full",
                    p.latest_run?.status === "failed"
                      ? "bg-red-500"
                      : "bg-amber-500",
                  )}
                  aria-hidden
                />
                {p.name}
                <span className="text-muted-foreground">{alertReason(p)}</span>
              </button>
            ))}
          </div>
        </div>
      ) : null}

      <div className="grid items-start gap-3 lg:grid-cols-2 xl:grid-cols-3">
        {sortedCells.map((cell) => {
          if (cell.kind === "single") {
            return (
              <PipelineCard
                key={cell.pipeline.id}
                nodeRef={setCardRef(cell.pipeline.name)}
                projectSlug={projectSlug}
                pipeline={cell.pipeline}
                edges={edges}
                runs={runs}
              />
            );
          }
          return (
            <ChainCell
              key={cell.pipelines[0]!.id}
              pipelines={cell.pipelines}
              projectSlug={projectSlug}
              edges={edges}
              runs={runs}
              setCardRef={setCardRef}
            />
          );
        })}
      </div>
    </div>
  );
}

// ChainCell stacks linear-chain pipelines vertically with a thin
// dashed connector on the left margin between cards. Reads as the
// timeline pattern in the user's reference: card → dashed segment
// → card → dashed segment → card. No SVG arrows, no overflowing
// connectors — the geometry is just CSS.
function ChainCell({
  pipelines,
  projectSlug,
  edges,
  runs,
  setCardRef,
}: {
  pipelines: PipelineSummary[];
  projectSlug: string;
  edges: PipelineEdge[];
  runs: RunSummary[];
  setCardRef: (name: string) => (el: HTMLElement | null) => void;
}) {
  return (
    <div className="flex flex-col">
      {pipelines.map((p, i) => (
        <Fragment key={p.id}>
          {i > 0 ? (
            <div
              aria-hidden
              className="ml-6 h-5 w-0 border-l-[2px] border-dashed border-muted-foreground/45"
            />
          ) : null}
          <PipelineCard
            nodeRef={setCardRef(p.name)}
            projectSlug={projectSlug}
            pipeline={p}
            edges={edges}
            runs={runs}
          />
        </Fragment>
      ))}
    </div>
  );
}

// isAlerting decides whether a pipeline shows up in the top alert
// strip. Failing/canceled latest runs always count; pipelines with
// healthy latest runs but a low historical pass rate do too — flaky
// CI is "needs attention" even when today happens to be green.
function isAlerting(p: PipelineSummary): boolean {
  const tone: StatusTone = p.latest_run
    ? statusTone(p.latest_run.status)
    : "neutral";
  if (tone === "failed" || tone === "canceled") return true;
  if (p.metrics && p.metrics.runs_considered >= 3 && p.metrics.success_rate < 0.7) {
    return true;
  }
  return false;
}

// alertWeight ranks a pipeline for "failing-first" sort. Higher =
// comes first.
function alertWeight(p: PipelineSummary): number {
  const tone: StatusTone = p.latest_run
    ? statusTone(p.latest_run.status)
    : "neutral";
  if (tone === "failed") return 4;
  if (tone === "canceled") return 3;
  if (p.metrics && p.metrics.runs_considered >= 3 && p.metrics.success_rate < 0.7) {
    return 2;
  }
  if (tone === "running" || tone === "queued") return 1;
  return 0;
}

function alertReason(p: PipelineSummary): string {
  const status = p.latest_run?.status;
  if (status === "failed" || status === "canceled") return status;
  if (p.metrics && p.metrics.runs_considered >= 3) {
    return `${Math.round(p.metrics.success_rate * 100)}%`;
  }
  return "";
}

type Cell =
  | { kind: "single"; pipeline: PipelineSummary }
  | { kind: "chain"; pipelines: PipelineSummary[] };

function cellName(c: Cell): string {
  return c.kind === "single" ? c.pipeline.name : c.pipelines[0]!.name;
}

function cellWeight(c: Cell): number {
  if (c.kind === "single") return alertWeight(c.pipeline);
  return Math.max(...c.pipelines.map(alertWeight));
}

// buildCells partitions the project's pipelines into rendering
// cells. A cell is:
//   - "single" — a standalone pipeline (no edges, fan-in, fan-out,
//     or whose dependencies don't form a strict chain), or
//   - "chain" — a sequence A → B → C where every link is 1-to-1
//     (B has exactly one upstream A, A has exactly one downstream
//     B, and so on). Chains render as a vertical stack with a thin
//     dashed connector between cards.
//
// Strict 1-to-1 is the only case where vertical stacking reads
// unambiguously. Fan-in (multiple upstream → one downstream) and
// fan-out (one upstream → multiple downstream) break the linear
// stack metaphor; those land as singles, with the upstream pill in
// each downstream's header naming the trigger source instead.
function buildCells(
  pipelines: PipelineSummary[],
  edges: PipelineEdge[],
): Cell[] {
  const names = new Set(pipelines.map((p) => p.name));
  const upstream = new Map<string, string[]>();
  const downstream = new Map<string, string[]>();
  for (const p of pipelines) {
    upstream.set(p.name, []);
    downstream.set(p.name, []);
  }
  for (const e of edges) {
    if (!names.has(e.from_pipeline) || !names.has(e.to_pipeline)) continue;
    upstream.get(e.to_pipeline)!.push(e.from_pipeline);
    downstream.get(e.from_pipeline)!.push(e.to_pipeline);
  }

  const pipelineByName = new Map(pipelines.map((p) => [p.name, p]));
  const consumed = new Set<string>();
  const cells: Cell[] = [];

  const sortedNames = [...pipelines]
    .sort((a, b) => a.name.localeCompare(b.name))
    .map((p) => p.name);

  for (const name of sortedNames) {
    if (consumed.has(name)) continue;

    // Walk upstream to find the head of any chain this pipeline
    // belongs to. Stop on fan-in, fan-out, or already-consumed.
    let head = name;
    while (true) {
      const ups = upstream.get(head)!;
      if (ups.length !== 1) break;
      const u = ups[0]!;
      if (downstream.get(u)!.length !== 1) break;
      if (consumed.has(u)) break;
      head = u;
    }

    // Walk downstream from head, collecting linear-chain links.
    const chain: string[] = [head];
    let cur = head;
    while (true) {
      const downs = downstream.get(cur)!;
      if (downs.length !== 1) break;
      const n = downs[0]!;
      if (upstream.get(n)!.length !== 1) break;
      if (consumed.has(n)) break;
      chain.push(n);
      cur = n;
    }
    for (const c of chain) consumed.add(c);

    if (chain.length === 1) {
      cells.push({ kind: "single", pipeline: pipelineByName.get(chain[0]!)! });
    } else {
      cells.push({
        kind: "chain",
        pipelines: chain.map((n) => pipelineByName.get(n)!),
      });
    }
  }

  return cells;
}
