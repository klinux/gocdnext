import Link from "next/link";
import type { Route } from "next";
import {
  ArrowDown,
  Check,
  ChevronsRight,
  Loader2,
  Minus,
  TriangleAlert,
  X,
} from "lucide-react";

import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import {
  durationBetween,
  formatDurationSeconds,
} from "@/lib/format";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { TriggerPipelineButton } from "@/components/pipelines/trigger-pipeline-button.client";
import { JobActionsMenu } from "@/components/pipelines/job-actions-menu.client";
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
  if (pipelines.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No pipelines yet. Push a YAML to the repo&apos;s config folder or run{" "}
        <code className="font-mono">gocdnext apply</code>.
      </p>
    );
  }

  const pipelinesByName = new Map(pipelines.map((p) => [p.name, p]));
  const layers = buildLayers(pipelines, edges);

  return (
    <div className="space-y-4">
      {layers.map((layer, layerIdx) => (
        <div key={`layer-${layerIdx}`} className="space-y-3">
          <div className="grid gap-3 lg:grid-cols-2">
            {layer.map((name) => {
              const pipeline = pipelinesByName.get(name);
              if (!pipeline) return null;
              return (
                <PipelineNode
                  key={pipeline.id}
                  projectSlug={projectSlug}
                  pipeline={pipeline}
                />
              );
            })}
          </div>
          {layerIdx < layers.length - 1 ? (
            <div className="flex justify-center" aria-hidden>
              <ArrowDown className="size-5 text-muted-foreground/50" />
            </div>
          ) : null}
        </div>
      ))}
    </div>
  );
}

function PipelineNode({
  projectSlug,
  pipeline,
}: {
  projectSlug: string;
  pipeline: PipelineSummary;
}) {
  const run = pipeline.latest_run;
  const columns = buildColumns(pipeline);

  return (
    <article className="flex flex-col gap-3 rounded-lg border bg-card p-3 shadow-sm">
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

// JobRow is clickable: wraps the status dot + name in a dropdown
// trigger so the user gets "View logs" / "Re-run pipeline" without
// navigating first. Runs that don't exist yet still get a trigger
// so the affordance is discoverable, but the items inside are
// disabled.
function JobRow({ job, runId }: { job: MergedJob; runId?: string }) {
  const tone: StatusTone = job.run ? statusTone(job.run.status) : "neutral";
  const label = job.run ? `${job.name} (${job.run.status})` : `${job.name} (not run)`;
  return (
    <li>
      <JobActionsMenu label={label} runId={runId}>
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
        <span className="truncate text-[11px] font-mono">{job.name}</span>
      </JobActionsMenu>
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
