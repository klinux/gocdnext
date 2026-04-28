"use client";

import {
  Fragment,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
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

// PipelineFlow renders all pipelines in a single dense grid with
// chain pipelines forced to column 1 (left edge). A separate
// left-margin column hosts a vertical dashed line + circular dots
// that mark each chain card's anchor — visually the chain reads
// like a timeline, the cards in cols 2..N flow naturally beside
// it. Independent pipelines (no upstream) inhabit the right cols
// and feel siblings to the chain rows.
export function PipelineFlow({ projectSlug, pipelines, edges, runs }: Props) {
  const cells = useMemo(
    () => buildCells(pipelines, edges),
    [pipelines, edges],
  );

  const cardRefs = useRef(new Map<string, HTMLElement>());
  const overlayRef = useRef<HTMLDivElement>(null);
  const [markers, setMarkers] = useState<ChainOverlayMarker[]>([]);

  if (pipelines.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No pipelines yet. Push a YAML to the repo&apos;s config folder or run{" "}
        <code className="font-mono">gocdnext apply</code>.
      </p>
    );
  }

  // Sort cells: chains first (anchor the left rail), then singles
  // by alert weight (failing → degraded → healthy), then alpha.
  const sortedCells = [...cells].sort((a, b) => {
    if (a.kind !== b.kind) return a.kind === "chain" ? -1 : 1;
    const wa = cellWeight(a);
    const wb = cellWeight(b);
    if (wa !== wb) return wb - wa;
    return cellName(a).localeCompare(cellName(b));
  });

  // Flatten cells into the DOM order the grid renders. Each chain
  // member becomes its own grid item with gridColumn=1; singles
  // back-fill via grid-auto-flow: dense.
  const orderedPipelines: { pipeline: PipelineSummary; chainId: number | null }[] = [];
  let chainId = 0;
  for (const cell of sortedCells) {
    if (cell.kind === "chain") {
      for (const p of cell.pipelines) {
        orderedPipelines.push({ pipeline: p, chainId });
      }
      chainId++;
    } else {
      orderedPipelines.push({ pipeline: cell.pipeline, chainId: null });
    }
  }

  const alerts = pipelines.filter(isAlerting);

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

  // Measure chain-card positions to draw the dashed line + dots in
  // the left-margin overlay column. Re-runs on resize and on cell
  // changes so wrap-driven row shifts (lg ↔ xl breakpoints) update
  // markers without manual intervention.
  const chainGroups = useMemo(() => {
    const map = new Map<number, string[]>();
    for (const { pipeline, chainId: cid } of orderedPipelines) {
      if (cid == null) continue;
      const arr = map.get(cid) ?? [];
      arr.push(pipeline.name);
      map.set(cid, arr);
    }
    return [...map.entries()].map(([id, names]) => ({ id, names }));
  }, [orderedPipelines]);

  useLayoutEffect(() => {
    const compute = () => {
      const overlay = overlayRef.current;
      if (!overlay) return;
      const overlayRect = overlay.getBoundingClientRect();
      const next: ChainOverlayMarker[] = [];
      for (const group of chainGroups) {
        const dots: number[] = [];
        let topY = Infinity;
        let bottomY = -Infinity;
        for (const name of group.names) {
          const el = cardRefs.current.get(name);
          if (!el) continue;
          const r = el.getBoundingClientRect();
          const dotY = r.top + r.height / 2 - overlayRect.top;
          dots.push(dotY);
          topY = Math.min(topY, dotY);
          bottomY = Math.max(bottomY, dotY);
        }
        if (dots.length >= 1 && Number.isFinite(topY)) {
          next.push({
            id: group.id,
            lineTop: topY,
            lineHeight: bottomY - topY,
            dotYs: dots,
          });
        }
      }
      setMarkers(next);
    };
    compute();
    const ro = new ResizeObserver(compute);
    if (overlayRef.current) ro.observe(overlayRef.current);
    for (const el of cardRefs.current.values()) ro.observe(el);
    window.addEventListener("resize", compute);
    return () => {
      ro.disconnect();
      window.removeEventListener("resize", compute);
    };
  }, [chainGroups]);

  return (
    <div className="space-y-4">
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

      <div className="flex items-stretch gap-2">
        {/* Left-margin overlay column. Holds the chain rail (dashed
            line + dots) without occupying grid space — width fixed
            at 16px so cards keep their natural proportions. */}
        <div ref={overlayRef} className="relative w-4 shrink-0" aria-hidden>
          {markers.map((m) => (
            <Fragment key={m.id}>
              <span
                className="absolute left-1.5 w-0 border-l-[2px] border-dashed border-muted-foreground/55"
                style={{ top: m.lineTop, height: m.lineHeight }}
              />
              {m.dotYs.map((y, i) => (
                <span
                  key={i}
                  className="absolute size-2 -translate-x-1/2 -translate-y-1/2 rounded-full bg-muted-foreground"
                  style={{ left: 8, top: y }}
                />
              ))}
            </Fragment>
          ))}
        </div>
        <div
          className="grid flex-1 items-start gap-3 lg:grid-cols-2 xl:grid-cols-3"
          style={{ gridAutoFlow: "row dense" }}
        >
          {orderedPipelines.map(({ pipeline, chainId }) => (
            <div
              key={pipeline.id}
              ref={(el) => setCardRef(pipeline.name)(el)}
              style={chainId != null ? { gridColumn: 1 } : undefined}
            >
              <PipelineCard
                projectSlug={projectSlug}
                pipeline={pipeline}
                edges={edges}
                runs={runs}
              />
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

type ChainOverlayMarker = {
  id: number;
  lineTop: number;
  lineHeight: number;
  dotYs: number[];
};

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
//     B, and so on). Chain members render as separate grid cells
//     forced to col 1, with a left-rail overlay tying them.
//
// Strict 1-to-1 is the only case where the chain rail reads
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

    let head = name;
    while (true) {
      const ups = upstream.get(head)!;
      if (ups.length !== 1) break;
      const u = ups[0]!;
      if (downstream.get(u)!.length !== 1) break;
      if (consumed.has(u)) break;
      head = u;
    }

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
