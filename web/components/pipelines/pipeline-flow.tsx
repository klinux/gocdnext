"use client";

import { useLayoutEffect, useMemo, useRef, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import type { Route } from "next";
import {
  Check,
  ChevronsRight,
  Loader2,
  Minus,
  RotateCcw,
  TriangleAlert,
  X,
} from "lucide-react";
import { toast } from "sonner";

import { rerunRun } from "@/server/actions/runs";

import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import {
  durationBetween,
  formatDurationSeconds,
} from "@/lib/format";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { TriggerPipelineButton } from "@/components/pipelines/trigger-pipeline-button.client";
import type {
  DefinitionJob,
  JobRunSummaryLite,
  PipelineEdge,
  PipelineSummary,
  StageRunSummary,
} from "@/types/api";

type Props = {
  projectSlug: string;
  pipelines: PipelineSummary[];
  edges: PipelineEdge[];
};

// PipelineFlow lays the project's pipelines out as a DAG: every
// upstream-material relationship pushes the downstream pipeline
// one layer deeper. Layers stack top-to-bottom so each layer reads
// as a horizontal row of sibling pipelines, with arrows between
// rows signalling trigger direction. Inside each pipeline card,
// stages flow left-to-right GitLab-style with their jobs stacked
// vertically inside each stage box — compact so multiple cards
// fit per row.
export function PipelineFlow({ projectSlug, pipelines, edges }: Props) {
  const pipelinesByName = useMemo(
    () => new Map(pipelines.map((p) => [p.name, p])),
    [pipelines],
  );
  const layers = useMemo(
    () => buildLayers(pipelines, edges),
    [pipelines, edges],
  );

  const containerRef = useRef<HTMLDivElement>(null);
  const cardRefs = useRef(new Map<string, HTMLElement>());
  const [paths, setPaths] = useState<EdgeGeometry[]>([]);

  // Effectful edges between cards are only drawn for real upstream
  // relationships — layers alone don't imply a connection.
  const renderableEdges = useMemo(() => {
    const names = new Set(pipelines.map((p) => p.name));
    return edges.filter(
      (e) => names.has(e.from_pipeline) && names.has(e.to_pipeline),
    );
  }, [pipelines, edges]);

  useLayoutEffect(() => {
    if (renderableEdges.length === 0) {
      setPaths([]);
      return;
    }
    const compute = () => {
      const container = containerRef.current;
      if (!container) return;
      const cRect = container.getBoundingClientRect();
      const next: EdgeGeometry[] = [];
      for (const e of renderableEdges) {
        const from = cardRefs.current.get(e.from_pipeline);
        const to = cardRefs.current.get(e.to_pipeline);
        if (!from || !to) continue;
        const f = from.getBoundingClientRect();
        const t = to.getBoundingClientRect();
        next.push({
          key: `${e.from_pipeline}->${e.to_pipeline}`,
          fromX: f.left + f.width / 2 - cRect.left,
          fromY: f.bottom - cRect.top,
          toX: t.left + t.width / 2 - cRect.left,
          toY: t.top - cRect.top,
          status: e.status ?? "",
        });
      }
      setPaths(next);
    };
    compute();
    const ro = new ResizeObserver(compute);
    if (containerRef.current) ro.observe(containerRef.current);
    for (const el of cardRefs.current.values()) ro.observe(el);
    window.addEventListener("resize", compute);
    return () => {
      ro.disconnect();
      window.removeEventListener("resize", compute);
    };
  }, [renderableEdges, layers]);

  if (pipelines.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No pipelines yet. Push a YAML to the repo&apos;s config folder or run{" "}
        <code className="font-mono">gocdnext apply</code>.
      </p>
    );
  }

  const setCardRef = (name: string) => (el: HTMLElement | null) => {
    if (el) cardRefs.current.set(name, el);
    else cardRefs.current.delete(name);
  };

  return (
    <div ref={containerRef} className="relative space-y-4">
      {paths.length > 0 ? (
        <svg
          aria-hidden
          className="pointer-events-none absolute inset-0 h-full w-full"
        >
          <defs>
            <marker
              id="dag-arrow-head"
              viewBox="0 0 10 10"
              refX="8"
              refY="5"
              markerWidth="6"
              markerHeight="6"
              orient="auto"
            >
              <path
                d="M 0 0 L 10 5 L 0 10 z"
                className="fill-muted-foreground/60"
              />
            </marker>
          </defs>
          {paths.map((p) => {
            const midY = (p.fromY + p.toY) / 2;
            return (
              <path
                key={p.key}
                d={`M ${p.fromX} ${p.fromY} C ${p.fromX} ${midY}, ${p.toX} ${midY}, ${p.toX} ${p.toY}`}
                className="fill-none stroke-muted-foreground/60"
                strokeWidth={1.5}
                markerEnd="url(#dag-arrow-head)"
              />
            );
          })}
        </svg>
      ) : null}

      {layers.map((layer, layerIdx) => (
        <div
          key={`layer-${layerIdx}`}
          // Extra top padding on downstream layers leaves room for
          // the connecting arc between rows so it doesn't hit the
          // card border. Keeps the curve visible even with tight
          // card heights.
          className={cn(layerIdx > 0 && "pt-6")}
        >
          <div className="grid gap-3 lg:grid-cols-2">
            {layer.map((name) => {
              const pipeline = pipelinesByName.get(name);
              if (!pipeline) return null;
              return (
                <PipelineNode
                  key={pipeline.id}
                  nodeRef={setCardRef(name)}
                  projectSlug={projectSlug}
                  pipeline={pipeline}
                />
              );
            })}
          </div>
        </div>
      ))}
    </div>
  );
}

type EdgeGeometry = {
  key: string;
  fromX: number;
  fromY: number;
  toX: number;
  toY: number;
  status: string;
};

function PipelineNode({
  projectSlug,
  pipeline,
  nodeRef,
}: {
  projectSlug: string;
  pipeline: PipelineSummary;
  // Registered by the PipelineFlow overlay so it can measure this
  // card's geometry and draw an SVG arrow to/from it. Optional
  // because other consumers (tests, snapshots) don't need edges.
  nodeRef?: (el: HTMLElement | null) => void;
}) {
  const run = pipeline.latest_run;
  const columns = buildColumns(pipeline);

  return (
    <article
      ref={nodeRef}
      className="flex flex-col gap-3 rounded-lg border bg-card p-3 shadow-sm"
    >
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0 flex-1">
          <div className="flex items-baseline gap-2">
            {run ? (
              <Link
                href={`/runs/${run.id}` as Route}
                className="truncate font-mono text-sm font-semibold hover:underline"
                title={`Open latest run (#${run.counter})`}
              >
                {pipeline.name}
              </Link>
            ) : (
              <h3 className="truncate font-mono text-sm font-semibold">
                {pipeline.name}
              </h3>
            )}
            <span className="text-[11px] text-muted-foreground">
              v{pipeline.definition_version}
            </span>
          </div>
          <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
            {run ? (
              <>
                <Link
                  href={`/runs/${run.id}` as Route}
                  className="font-mono text-foreground hover:underline"
                >
                  #{run.counter}
                </Link>
                <StatusBadge status={run.status} className="text-[10px]" />
                <span>
                  {formatDurationSeconds(
                    durationBetween(run.started_at, run.finished_at),
                  )}
                </span>
                <span>
                  · <RelativeTime at={run.started_at ?? run.created_at} />
                </span>
              </>
            ) : (
              <span className="italic">Never run</span>
            )}
          </div>
        </div>
        <TriggerPipelineButton
          pipelineId={pipeline.id}
          pipelineName={pipeline.name}
          projectSlug={projectSlug}
        />
      </div>

      {columns.length > 0 ? (
        <div className="-mx-1 overflow-x-auto">
          <div className="flex items-stretch gap-1.5 px-1">
            {columns.map((col, i) => (
              <StageColumnBox
                key={`${col.name}-${i}`}
                column={col}
                runId={run?.id}
                isLast={i === columns.length - 1}
              />
            ))}
          </div>
        </div>
      ) : (
        <p className="text-xs text-muted-foreground">No stages defined yet.</p>
      )}
    </article>
  );
}

type StageColumn = {
  name: string;
  run?: StageRunSummary;
  jobs: MergedJob[];
};

type MergedJob = {
  key: string;
  name: string;
  run?: JobRunSummaryLite;
};

function buildColumns(pipeline: PipelineSummary): StageColumn[] {
  const runStages = pipeline.latest_run_stages ?? [];
  const defStages = pipeline.definition_stages ?? [];
  const defJobs = pipeline.definition_jobs ?? [];
  const runByName = new Map(runStages.map((s) => [s.name, s]));
  const defJobsByStage = groupBy(defJobs, (j) => j.stage);

  const orderedStageNames =
    defStages.length > 0 ? defStages : runStages.map((s) => s.name);

  return orderedStageNames.map((name) => {
    const stageRun = runByName.get(name);
    return {
      name,
      run: stageRun,
      jobs: mergeJobs(stageRun?.jobs ?? [], defJobsByStage.get(name) ?? []),
    };
  });
}

function mergeJobs(
  runtime: JobRunSummaryLite[],
  def: DefinitionJob[],
): MergedJob[] {
  const runtimeByName = new Map(runtime.map((j) => [j.name, j]));
  if (def.length > 0) {
    return def.map((j) => ({
      key: j.name,
      name: j.name,
      run: runtimeByName.get(j.name),
    }));
  }
  return runtime.map((j) => ({ key: j.id, name: j.name, run: j }));
}

function groupBy<T, K>(items: T[], keyFn: (t: T) => K): Map<K, T[]> {
  const out = new Map<K, T[]>();
  for (const item of items) {
    const k = keyFn(item);
    const bucket = out.get(k);
    if (bucket) bucket.push(item);
    else out.set(k, [item]);
  }
  return out;
}

// StageColumnBox is the GitLab-style bordered column. Header
// shows stage name + status icon; body lists jobs as compact
// rows with dropdowns. Width is bounded so four or five stages
// can fit before the container scrolls.
function StageColumnBox({
  column,
  runId,
  isLast,
}: {
  column: StageColumn;
  runId?: string;
  isLast: boolean;
}) {
  const tone: StatusTone = column.run
    ? statusTone(column.run.status)
    : "neutral";
  return (
    <div className="flex items-stretch">
      <div
        className={cn(
          "flex w-[150px] shrink-0 flex-col gap-1 rounded-md border bg-background p-1.5",
          stageColumnClasses[tone],
        )}
      >
        <div className="flex items-center justify-between gap-1">
          <span className="truncate text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
            {column.name}
          </span>
          <span
            className={cn(
              "inline-flex size-3.5 shrink-0 items-center justify-center rounded-full",
              stageDotClasses[tone],
              column.run?.status === "running" && "animate-pulse",
            )}
            title={column.run ? column.run.status : "not run"}
            aria-hidden
          >
            <StageIcon tone={tone} />
          </span>
        </div>
        {column.jobs.length > 0 ? (
          <ul className="space-y-0.5">
            {column.jobs.map((j) => (
              <JobRow key={j.key} job={j} runId={runId} />
            ))}
          </ul>
        ) : (
          <p className="text-[10px] italic text-muted-foreground/70">
            No jobs
          </p>
        )}
      </div>
      {!isLast ? (
        <span
          aria-hidden
          className="mx-0.5 my-auto inline-block h-px w-2 bg-muted-foreground/30"
        />
      ) : null}
    </div>
  );
}

// JobRow adopts the GitLab CI layout: status dot + clickable
// job name (deep-links to the specific job on the run detail
// page) + retry icon aligned right. Runs that don't exist yet
// render the name as plain text with no retry affordance so the
// affordance only appears when the action is meaningful.
function JobRow({ job, runId }: { job: MergedJob; runId?: string }) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const tone: StatusTone = job.run ? statusTone(job.run.status) : "neutral";
  const jobHref =
    runId && job.run
      ? (`/runs/${runId}#job-${job.run.id}` as Route)
      : null;

  const retry = () => {
    if (!runId) return;
    startTransition(async () => {
      const res = await rerunRun({ runId });
      if (!res.ok) {
        toast.error(`Re-run failed: ${res.error}`);
        return;
      }
      const newID = String(res.data.run_id ?? "");
      toast.success(`Re-ran ${job.name}`, {
        action: newID
          ? {
              label: "Open",
              onClick: () => router.push(`/runs/${newID}` as Route),
            }
          : undefined,
      });
    });
  };

  return (
    <li className="flex items-center gap-1.5">
      <span
        className={cn(
          "inline-flex size-3 shrink-0 items-center justify-center rounded-full",
          stageDotClasses[tone],
          job.run?.status === "running" && "animate-pulse",
        )}
        aria-hidden
      >
        <JobIcon tone={tone} />
      </span>
      {jobHref ? (
        <Link
          href={jobHref}
          className="min-w-0 flex-1 truncate font-mono text-[11px] hover:underline"
          title={`Open logs for ${job.name}`}
        >
          {job.name}
        </Link>
      ) : (
        <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-muted-foreground">
          {job.name}
        </span>
      )}
      {runId ? (
        <button
          type="button"
          onClick={(e) => {
            // Stop propagation so a future click handler on the
            // stage box doesn't get a surprise event — the button
            // is the only interactive target at this level.
            e.preventDefault();
            e.stopPropagation();
            retry();
          }}
          disabled={pending}
          aria-label={`Re-run pipeline for ${job.name}`}
          title="Re-run this commit"
          className="inline-flex size-4 shrink-0 items-center justify-center rounded-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-50"
        >
          <RotateCcw className={cn("size-2.5", pending && "animate-spin")} />
        </button>
      ) : null}
    </li>
  );
}

function StageIcon({ tone }: { tone: StatusTone }) {
  return <ToneIcon tone={tone} className="size-2.5" />;
}

function JobIcon({ tone }: { tone: StatusTone }) {
  return <ToneIcon tone={tone} className="size-2" />;
}

function ToneIcon({
  tone,
  className,
}: {
  tone: StatusTone;
  className: string;
}) {
  switch (tone) {
    case "success":
      return <Check className={className} aria-hidden strokeWidth={3} />;
    case "failed":
      return <X className={className} aria-hidden strokeWidth={3} />;
    case "running":
      return <Loader2 className={cn(className, "animate-spin")} aria-hidden />;
    case "queued":
    case "warning":
      return <TriangleAlert className={className} aria-hidden />;
    case "canceled":
      return <Minus className={className} aria-hidden strokeWidth={3} />;
    case "skipped":
    case "neutral":
    default:
      return (
        <ChevronsRight className={className} aria-hidden strokeWidth={2.5} />
      );
  }
}

function buildLayers(
  pipelines: PipelineSummary[],
  edges: PipelineEdge[],
): string[][] {
  const names = new Set(pipelines.map((p) => p.name));
  const inDegree = new Map<string, number>();
  const forward = new Map<string, string[]>();
  for (const p of pipelines) {
    inDegree.set(p.name, 0);
    forward.set(p.name, []);
  }
  for (const e of edges) {
    if (!names.has(e.from_pipeline) || !names.has(e.to_pipeline)) continue;
    inDegree.set(e.to_pipeline, (inDegree.get(e.to_pipeline) ?? 0) + 1);
    forward.get(e.from_pipeline)!.push(e.to_pipeline);
  }

  const layer = new Map<string, number>();
  const queue: string[] = [];
  for (const name of inDegree.keys()) {
    if ((inDegree.get(name) ?? 0) === 0) {
      layer.set(name, 0);
      queue.push(name);
    }
  }
  while (queue.length > 0) {
    const u = queue.shift()!;
    for (const v of forward.get(u) ?? []) {
      const next = Math.max(layer.get(v) ?? 0, (layer.get(u) ?? 0) + 1);
      layer.set(v, next);
      inDegree.set(v, (inDegree.get(v) ?? 0) - 1);
      if ((inDegree.get(v) ?? 0) === 0) queue.push(v);
    }
  }
  for (const p of pipelines) {
    if (!layer.has(p.name)) layer.set(p.name, 0);
  }

  const maxLayer = Math.max(0, ...Array.from(layer.values()));
  const out: string[][] = Array.from({ length: maxLayer + 1 }, () => []);
  const sorted = [...pipelines].sort((a, b) => a.name.localeCompare(b.name));
  for (const p of sorted) {
    out[layer.get(p.name) ?? 0]!.push(p.name);
  }
  return out;
}

// Stage box border tint — matches GitLab's "status on frame" cue.
// Neutral (never run) stays on the default border so the UI doesn't
// scream about pipelines that simply haven't triggered yet.
const stageColumnClasses: Record<StatusTone, string> = {
  success: "border-emerald-500/30",
  failed: "border-red-500/50",
  running: "border-sky-500/50",
  queued: "border-amber-500/40",
  warning: "border-amber-500/40",
  canceled: "border-muted-foreground/30",
  skipped: "border-muted-foreground/20",
  neutral: "border-border",
};

const stageDotClasses: Record<StatusTone, string> = {
  success: "bg-emerald-500 text-white",
  failed: "bg-red-500 text-white",
  running: "bg-sky-500 text-white",
  queued: "bg-amber-500 text-white",
  warning: "bg-amber-500 text-white",
  canceled: "bg-muted-foreground/60 text-background",
  skipped: "bg-muted text-muted-foreground border border-muted-foreground/30",
  neutral: "bg-muted text-muted-foreground border border-muted-foreground/30",
};
