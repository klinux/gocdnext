"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import { Activity, AlertTriangle, CheckCircle2, Clock, GitBranch } from "lucide-react";

import { LiveDuration } from "@/components/shared/live-duration";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { TriggerPipelineButton } from "@/components/pipelines/trigger-pipeline-button.client";
import { PipelineStageRow } from "@/components/pipelines/pipeline-stage-row";
import { PipelineOverviewSheet } from "@/components/pipelines/pipeline-overview-sheet.client";
import {
  buildColumns,
  pickBottleneck,
  type Bottleneck,
} from "@/components/pipelines/pipeline-card-helpers";
import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import { formatDurationSeconds } from "@/lib/format";
import type {
  PipelineEdge,
  PipelineMetrics,
  PipelineSummary,
  RunSummary,
} from "@/types/api";

type Props = {
  projectSlug: string;
  pipeline: PipelineSummary;
  // Edges + runs flow through from PipelineFlow so the overview
  // sheet can render upstream/downstream + recent runs without
  // a second fetch — everything is already in ProjectDetail.
  edges: PipelineEdge[];
  runs: RunSummary[];
  // Registered by the PipelineFlow overlay so it can measure this
  // card's geometry and draw an SVG arrow to/from it.
  nodeRef?: (el: HTMLElement | null) => void;
};

// PipelineCard is the bordered container for one pipeline. Jobs are
// the prominent visual cards inside (see pipeline-stage-row); this
// wrapper gives the pipeline itself a card identity — border, bg,
// and a divider under the header so the stage flow reads as a
// separate region. Click the header to open the pipeline overview
// sheet (KPIs + details + triggered-by + recent runs).
export function PipelineCard({
  projectSlug,
  pipeline,
  edges,
  runs,
  nodeRef,
}: Props) {
  const run = pipeline.latest_run;
  const meta = pipeline.latest_run_meta;
  const columns = useMemo(() => buildColumns(pipeline), [pipeline]);
  const bottleneck = useMemo(() => pickBottleneck(columns), [columns]);
  const metrics = pipeline.metrics;
  const [overviewOpen, setOverviewOpen] = useState(false);

  const commitSubject = meta?.message ? firstLine(meta.message) : null;
  const shortSha = meta?.revision ? meta.revision.slice(0, 7) : null;

  const tone: StatusTone = run ? statusTone(run.status) : "neutral";
  return (
    <>
      <article
        ref={nodeRef}
        className={cn(
          "flex flex-col overflow-hidden rounded-lg border bg-card shadow-sm",
          "border-l-4",
          borderToneClasses[tone],
        )}
      >
        <header className="flex flex-col gap-1 border-b border-border px-3 py-2">
          {/* Top line: status dot + pipeline name + branch + Run latest.
              Everything that competes for "what is this" goes here;
              meta (version, run #, ago) demotes to a muted line below. */}
          <div className="flex items-center gap-2">
            <span
              className={cn(
                "inline-block size-2.5 shrink-0 rounded-full",
                dotToneClasses[tone],
                run?.status === "running" && "animate-pulse",
              )}
              aria-hidden
              title={run?.status ?? "not run"}
            />
            <button
              type="button"
              onClick={() => setOverviewOpen(true)}
              title="Open pipeline overview"
              className="min-w-0 flex-1 truncate rounded-sm text-left font-mono text-sm font-semibold outline-none hover:underline focus-visible:ring-2 focus-visible:ring-ring"
            >
              {pipeline.name}
            </button>
            {bottleneck ? (
              <BottleneckPill bottleneck={bottleneck} />
            ) : null}
            {meta?.branch ? (
              <span
                className="inline-flex max-w-[160px] items-center gap-1 rounded-full bg-muted px-2 py-0.5 font-mono text-[10px] text-muted-foreground"
                title={`Ref: ${meta.branch}`}
              >
                <GitBranch className="size-3" aria-hidden />
                <span className="truncate">{meta.branch}</span>
              </span>
            ) : null}
            <TriggerPipelineButton
              pipelineId={pipeline.id}
              pipelineName={pipeline.name}
              projectSlug={projectSlug}
            />
          </div>

          {/* Bottom line: muted run meta + commit subject. Single
              row, ellipsised — the overview sheet has the full
              breakdown for anyone who needs it. */}
          <div className="flex items-center gap-x-2 text-[11px] text-muted-foreground">
            <span className="font-mono text-[10px]">v{pipeline.definition_version}</span>
            {run ? (
              <>
                <Link
                  href={`/runs/${run.id}` as Route}
                  className="font-mono text-foreground hover:underline"
                >
                  #{run.counter}
                </Link>
                <LiveDuration
                  startedAt={run.started_at}
                  finishedAt={run.finished_at}
                  className="font-mono tabular-nums"
                />
                <span>
                  <RelativeTime at={run.started_at ?? run.created_at} />
                </span>
              </>
            ) : (
              <span className="italic">Never run</span>
            )}
            {shortSha ? (
              <span className="font-mono text-[10px]">{shortSha}</span>
            ) : null}
            {commitSubject ? (
              <span
                className="truncate text-foreground/80"
                title={meta?.message}
              >
                {commitSubject}
              </span>
            ) : null}
            {run ? (
              <StatusBadge status={run.status} className="ml-auto text-[10px]" />
            ) : null}
          </div>
        </header>

        <div className="flex flex-1 flex-col">
          <PipelineStageRow columns={columns} runId={run?.id} />
        </div>

        {metrics ? <InlineMetricsFooter metrics={metrics} /> : null}
      </article>

      <PipelineOverviewSheet
        open={overviewOpen}
        onOpenChange={setOverviewOpen}
        pipeline={pipeline}
        edges={edges}
        runs={runs}
      />
    </>
  );
}

// borderToneClasses maps the latest run's status onto a left-edge
// colour so failing pipelines pop in a dense grid without the user
// reading every label. Healthy + neutral states keep the default
// border so green doesn't shout the same as red.
const borderToneClasses: Record<StatusTone, string> = {
  success: "border-border",
  failed: "border-l-red-500",
  running: "border-l-sky-500",
  queued: "border-l-amber-500",
  warning: "border-l-amber-500",
  awaiting: "border-l-amber-500",
  canceled: "border-l-muted-foreground/40",
  skipped: "border-border",
  neutral: "border-border",
};

const dotToneClasses: Record<StatusTone, string> = {
  success: "bg-emerald-500",
  failed: "bg-red-500",
  running: "bg-sky-500",
  queued: "bg-amber-500",
  warning: "bg-amber-500",
  awaiting: "bg-amber-500",
  canceled: "bg-muted-foreground/60",
  skipped: "bg-muted border border-muted-foreground/30",
  neutral: "bg-muted border border-muted-foreground/30",
};

// BottleneckPill condenses the (formerly bottom-strip) callout into
// a single header chip. Same data — slowest stage + the symptom —
// surfaced where the operator's eye already lands when scanning.
function BottleneckPill({ bottleneck }: { bottleneck: Bottleneck }) {
  const detail: string[] = [];
  if (bottleneck.successRate != null) {
    detail.push(`${Math.round(bottleneck.successRate * 100)}% C/A`);
  } else if (bottleneck.overP50Sec != null) {
    detail.push(`+${formatDurationSeconds(bottleneck.overP50Sec)} p50`);
  }
  return (
    <span
      className="inline-flex shrink-0 items-center gap-1 rounded-full border border-amber-500/30 bg-amber-500/10 px-2 py-0.5 text-[10px] text-amber-700 dark:text-amber-400"
      title={`${bottleneck.stageName} is the bottleneck`}
    >
      <AlertTriangle className="size-3" aria-hidden />
      <span className="font-mono font-semibold">{bottleneck.stageName}</span>
      {detail.length > 0 ? <span>· {detail.join(" · ")}</span> : null}
    </span>
  );
}

// InlineMetricsFooter is the redesigned bottom strip: one row, no
// big-icon ceremony, footer-coloured background to read as
// "context". Drops the per-cell labels in favour of a single legend
// (LEAD / PROC / SR) since the operator already knows what those
// abbreviations mean from every other CI tool.
function InlineMetricsFooter({ metrics }: { metrics: PipelineMetrics }) {
  if (metrics.runs_considered === 0) {
    return null;
  }
  const rate = Math.round(metrics.success_rate * 100);
  return (
    <div className="flex items-center gap-3 border-t border-border bg-muted/30 px-3 py-1.5 text-[10px] text-muted-foreground">
      <span className="inline-flex items-center gap-1">
        <Clock className="size-3" aria-hidden />
        <span className="uppercase tracking-wide">Lead</span>
        <span className="font-mono text-foreground tabular-nums">
          {formatDurationSeconds(metrics.lead_time_p50_seconds)}
        </span>
      </span>
      <span className="inline-flex items-center gap-1">
        <Activity className="size-3" aria-hidden />
        <span className="uppercase tracking-wide">Proc</span>
        <span className="font-mono text-foreground tabular-nums">
          {formatDurationSeconds(metrics.process_time_p50_seconds)}
        </span>
      </span>
      <span className="inline-flex items-center gap-1">
        <CheckCircle2 className="size-3" aria-hidden />
        <span
          className={cn(
            "font-mono tabular-nums",
            rate >= 90
              ? "text-emerald-500"
              : rate >= 70
                ? "text-amber-500"
                : "text-red-500",
          )}
        >
          {rate}%
        </span>
      </span>
      <span className="ml-auto font-mono tabular-nums">
        {metrics.runs_considered} runs · {metrics.window_days}d
      </span>
    </div>
  );
}

// firstLine returns the commit message's subject line only —
// anything after the first newline is body that doesn't fit on
// one visual row. Falls through to the original when it's already
// short enough.
function firstLine(message: string): string {
  const idx = message.indexOf("\n");
  return idx >= 0 ? message.slice(0, idx) : message;
}
