"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import {
  Activity,
  AlertTriangle,
  CheckCircle2,
  Clock,
  GitBranch,
} from "lucide-react";

import { EntityChip } from "@/components/shared/entity-chip";
import { LiveDuration } from "@/components/shared/live-duration";
import { RelativeTime } from "@/components/shared/relative-time";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { TriggerPipelineButton } from "@/components/pipelines/trigger-pipeline-button.client";
import { PipelineStageStrip } from "@/components/pipelines/pipeline-stage-strip";
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
  // Upstream edges this card receives — surface as small pills in
  // the header so the operator sees "this pipeline runs after X
  // passes" without having to read the SVG arrow above. Caps at
  // two pills; rare 3+-fan-in cases hide the rest behind a count.
  const upstreams = useMemo(
    () => edges.filter((e) => e.to_pipeline === pipeline.name),
    [edges, pipeline.name],
  );
  // Downstream edges this card feeds — pipelines that name THIS one
  // as their upstream material. Same scan trick as upstreams: lets
  // the operator answer "what does this pipeline trigger?" inline,
  // without having to open the overview sheet's Triggers tab.
  const downstreams = useMemo(
    () => edges.filter((e) => e.from_pipeline === pipeline.name),
    [edges, pipeline.name],
  );
  const bottleneck = useMemo(() => pickBottleneck(columns), [columns]);
  const metrics = pipeline.metrics;
  const [overviewOpen, setOverviewOpen] = useState(false);

  const commitSubject = meta?.message ? truncate(firstLine(meta.message), 30) : null;
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
          tone === "running" && "animate-running-border",
        )}
      >
        <header className="flex flex-col gap-0.5 border-b border-border px-2.5 py-1.5">
          {/* Top line: pipeline name + branch + Run latest. Status
              is conveyed by the coloured left border of the card and
              the job circles in the stage strip below — the explicit
              text/icon badge that used to live here was redundant. */}
          <div className="flex items-center gap-2">
            <Tooltip>
              <TooltipTrigger
                render={
                  <button
                    type="button"
                    onClick={() => setOverviewOpen(true)}
                    className="min-w-0 flex-1 truncate rounded-sm text-left font-mono text-sm font-semibold outline-none hover:underline focus-visible:ring-2 focus-visible:ring-ring"
                  />
                }
              >
                {pipeline.name}
              </TooltipTrigger>
              <TooltipContent align="start">
                Open pipeline overview
              </TooltipContent>
            </Tooltip>
            {bottleneck ? (
              <BottleneckPill bottleneck={bottleneck} />
            ) : null}
            {meta?.branch ? (
              <Tooltip>
                <TooltipTrigger
                  render={
                    <span className="inline-flex max-w-[160px] items-center gap-1 rounded-full bg-muted px-2 py-0.5 font-mono text-[10px] text-muted-foreground" />
                  }
                >
                  <GitBranch className="size-3" aria-hidden />
                  <span className="truncate">{meta.branch}</span>
                </TooltipTrigger>
                <TooltipContent>Ref: {meta.branch}</TooltipContent>
              </Tooltip>
            ) : null}
            <TriggerPipelineButton
              pipelineId={pipeline.id}
              pipelineName={pipeline.name}
              projectSlug={projectSlug}
              currentStatus={run?.status}
            />
          </div>

          {/* Bottom line: muted run meta + commit subject. Single
              row, ellipsised — the overview sheet has the full
              breakdown for anyone who needs it. */}
          <div className="flex items-baseline gap-x-2 text-[11px] leading-tight text-muted-foreground">
            <span className="font-mono">v{pipeline.definition_version}</span>
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
            {shortSha ? <span className="font-mono">{shortSha}</span> : null}
            {commitSubject ? (
              meta?.message && meta.message !== commitSubject ? (
                <Tooltip>
                  <TooltipTrigger
                    render={
                      <span className="truncate text-foreground/80" />
                    }
                  >
                    {commitSubject}
                  </TooltipTrigger>
                  <TooltipContent className="max-w-md whitespace-pre-wrap">
                    {meta.message}
                  </TooltipContent>
                </Tooltip>
              ) : (
                <span className="truncate text-foreground/80">
                  {commitSubject}
                </span>
              )
            ) : null}
          </div>
        </header>

        <div className="flex flex-1 flex-col">
          <PipelineStageStrip columns={columns} runId={run?.id} />
        </div>

        {metrics || upstreams.length > 0 || downstreams.length > 0 ? (
          <InlineMetricsFooter
            metrics={metrics}
            upstreams={upstreams}
            downstreams={downstreams}
          />
        ) : null}
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

// borderToneClasses maps the latest run's status onto the left
// edge colour. Failures + running stay full-saturated so they grab
// attention; success uses a softer emerald so it reads "ok" at a
// glance without the page becoming a wall of green; never-run /
// skipped fall back to the default border so they sit quietly.
const borderToneClasses: Record<StatusTone, string> = {
  success: "border-l-emerald-500/70",
  failed: "border-l-red-500",
  running: "border-l-sky-500",
  queued: "border-l-amber-500",
  warning: "border-l-amber-500",
  awaiting: "border-l-amber-500",
  canceled: "border-l-muted-foreground/60",
  skipped: "border-l-muted-foreground/40",
  neutral: "border-border",
};

// pipelineStageHint formats the upstream's stage name for the
// relationship chip's tooltip — kept as a single helper so the
// in/out tooltip strings stay parallel.
function pipelineStageHint(stage: string): string {
  return stage;
}

// RelationshipPills surfaces the cross-pipeline relationships
// flowing in or out of this card. Same chip language used on the
// run page (UpstreamBanner) and audit log (target column) so the
// operator's eye recognises the pattern across surfaces. Caps at
// 2 visible; rare wider fan-in/out hides the rest behind a "+N"
// overflow with the full list in a tooltip.
function RelationshipPills({
  edges,
  direction,
}: {
  edges: PipelineEdge[];
  direction: "in" | "out";
}) {
  const visible = edges.slice(0, 2);
  const overflow = edges.slice(2);
  // direction=in → upstream feeds; the chip names the FROM pipeline
  // direction=out → downstream feeds; the chip names the TO pipeline
  const labelFor = (e: PipelineEdge) =>
    direction === "in" ? e.from_pipeline : e.to_pipeline;
  const titleFor = (e: PipelineEdge) =>
    direction === "in"
      ? e.stage
        ? `Triggered when ${e.from_pipeline}.${e.stage} passes`
        : `Triggered by ${e.from_pipeline}`
      : e.stage
        ? `Triggers ${e.to_pipeline} after ${pipelineStageHint(e.stage)} passes`
        : `Triggers ${e.to_pipeline}`;
  return (
    <span className="flex flex-wrap items-center gap-1">
      {visible.map((e) => (
        <EntityChip
          key={`${direction}-${labelFor(e)}:${e.stage ?? ""}`}
          kind="pipeline"
          label={labelFor(e)}
          hint={e.stage ? `.${e.stage}` : undefined}
          direction={direction}
          title={titleFor(e)}
        />
      ))}
      {overflow.length > 0 ? (
        <Tooltip>
          <TooltipTrigger
            render={
              <span className="cursor-help font-mono text-[10px] text-sky-700/70 dark:text-sky-400/70" />
            }
          >
            +{overflow.length}
          </TooltipTrigger>
          <TooltipContent>
            <ul className="space-y-0.5">
              {overflow.map((e) => (
                <li key={`${direction}-${labelFor(e)}:${e.stage ?? ""}`}>
                  {labelFor(e)}
                  {e.stage ? `.${e.stage}` : ""}
                </li>
              ))}
            </ul>
          </TooltipContent>
        </Tooltip>
      ) : null}
    </span>
  );
}

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
    <Tooltip>
      <TooltipTrigger
        render={
          <span className="inline-flex shrink-0 cursor-help items-center gap-1 rounded-full border border-amber-500/30 bg-amber-500/10 px-2 py-0.5 text-[10px] text-amber-700 dark:text-amber-400" />
        }
      >
        <AlertTriangle className="size-3" aria-hidden />
        <span className="font-mono font-semibold">{bottleneck.stageName}</span>
        {detail.length > 0 ? <span>· {detail.join(" · ")}</span> : null}
      </TooltipTrigger>
      <TooltipContent>{bottleneck.stageName} is the bottleneck</TooltipContent>
    </Tooltip>
  );
}

// InlineMetricsFooter is the redesigned bottom strip: one row, no
// big-icon ceremony, footer-coloured background to read as
// "context". Drops the per-cell labels in favour of a single legend
// (LEAD / PROC / SR) since the operator already knows what those
// abbreviations mean from every other CI tool.
//
// Relationship pills sit here (rather than the header) because
// they're "context about this pipeline" the operator only consults
// when they care about the chain — same job the metrics row does.
// Both directions render: incoming triggers (upstreams) show on
// the left of the metrics; outgoing (downstreams) show on the
// right. The chain rail in the left margin already carries the
// at-a-glance signal "this has dependencies", so the header
// doesn't need a pill repeating the same fact.
function InlineMetricsFooter({
  metrics,
  upstreams,
  downstreams,
}: {
  metrics: PipelineMetrics | undefined;
  upstreams: PipelineEdge[];
  downstreams: PipelineEdge[];
}) {
  const hasMetrics = metrics != null && metrics.runs_considered > 0;
  const hasUpstreams = upstreams.length > 0;
  const hasDownstreams = downstreams.length > 0;
  if (!hasMetrics && !hasUpstreams && !hasDownstreams) {
    return null;
  }
  const rate = hasMetrics ? Math.round(metrics!.success_rate * 100) : 0;
  return (
    <div className="flex items-center gap-2.5 border-t border-border bg-muted/30 px-2.5 py-1 text-[10px] text-muted-foreground">
      {hasMetrics ? (
        <>
          <span className="inline-flex items-center gap-1">
            <Clock className="size-3" aria-hidden />
            <span className="uppercase tracking-wide">Lead</span>
            <span className="font-mono text-foreground tabular-nums">
              {formatDurationSeconds(metrics!.lead_time_p50_seconds)}
            </span>
          </span>
          <span className="inline-flex items-center gap-1">
            <Activity className="size-3" aria-hidden />
            <span className="uppercase tracking-wide">Proc</span>
            <span className="font-mono text-foreground tabular-nums">
              {formatDurationSeconds(metrics!.process_time_p50_seconds)}
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
        </>
      ) : null}
      <div className="ml-auto flex items-center gap-2">
        {hasUpstreams ? (
          <RelationshipPills edges={upstreams} direction="in" />
        ) : null}
        {hasDownstreams ? (
          <RelationshipPills edges={downstreams} direction="out" />
        ) : null}
        {hasMetrics ? (
          <span className="font-mono tabular-nums">
            {metrics!.runs_considered} runs · {metrics!.window_days}d
          </span>
        ) : null}
      </div>
    </div>
  );
}

// truncate clips a string at maxLen with a real ellipsis. CSS
// `truncate` already handles overflow visually, but it kicks in at
// container width and depends on flex layout — we want a hard cap
// at the source so the commit subject doesn't push other header
// elements (status pill, branch, Run latest) off-screen even
// before the truncate class can take over.
function truncate(s: string, maxLen: number): string {
  if (s.length <= maxLen) return s;
  return s.slice(0, maxLen).trimEnd() + "…";
}

// firstLine returns the commit message's subject line only —
// anything after the first newline is body that doesn't fit on
// one visual row. Falls through to the original when it's already
// short enough.
function firstLine(message: string): string {
  const idx = message.indexOf("\n");
  return idx >= 0 ? message.slice(0, idx) : message;
}
