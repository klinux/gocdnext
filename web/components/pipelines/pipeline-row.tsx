"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import {
  AlertTriangle,
  Check,
  GitBranch,
  Loader2,
  Minus,
  ShieldCheck,
  X,
} from "lucide-react";

import { cn } from "@/lib/utils";
import { isCompliancePipeline } from "@/lib/compliance";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { CauseBadge } from "@/components/shared/cause-badge";
import { LiveDuration } from "@/components/shared/live-duration";
import { RelativeTime } from "@/components/shared/relative-time";
import { TriggerPipelineButton } from "@/components/pipelines/trigger-pipeline-button.client";
import { JobActions } from "@/components/pipelines/job-actions.client";
import { ServicesCluster } from "@/components/pipelines/services-cluster";
import { PipelineOverviewSheet } from "@/components/pipelines/pipeline-overview-sheet.client";
import {
  buildColumns,
  pickBottleneck,
  type Bottleneck,
  type MergedJob,
  type StageColumn,
} from "@/components/pipelines/pipeline-card-helpers";
import { statusTone, type StatusTone } from "@/lib/status";
import { formatDurationSeconds } from "@/lib/format";
import type { PipelineEdge, PipelineSummary, RunSummary } from "@/types/api";

type Props = {
  projectSlug: string;
  pipeline: PipelineSummary;
  edges: PipelineEdge[];
  runs: RunSummary[];
  // flow tracks show the rail node; flat (Lista) mode drops it — same row,
  // no chain chrome.
  showRail: boolean;
};

// nodeBorder maps a run status to the chain-node ring + stage-box border.
// Mirrors the card's left-border language: failures/running saturated,
// success a calmer emerald, idle/never-run muted.
const nodeBorder: Record<StatusTone, string> = {
  success: "border-emerald-500",
  failed: "border-red-500",
  running: "border-sky-500",
  queued: "border-amber-500",
  warning: "border-amber-500",
  awaiting: "border-amber-500",
  canceled: "border-muted-foreground/60",
  skipped: "border-muted-foreground/40",
  neutral: "border-muted-foreground/40",
};

// rowAccent is the status-colored left edge of the row (inset shadow),
// restoring the old card's left-border language — failures/running
// saturated, success a calmer emerald, idle/never-run none.
const rowAccent: Record<StatusTone, string> = {
  success: "shadow-[inset_3px_0_0_0_var(--color-emerald-500)]",
  failed: "shadow-[inset_3px_0_0_0_var(--color-red-500)]",
  canceled: "shadow-[inset_3px_0_0_0_var(--color-red-500)]",
  running: "shadow-[inset_3px_0_0_0_var(--color-sky-500)]",
  queued: "shadow-[inset_3px_0_0_0_var(--color-amber-500)]",
  awaiting: "shadow-[inset_3px_0_0_0_var(--color-amber-500)]",
  warning: "shadow-[inset_3px_0_0_0_var(--color-amber-500)]",
  skipped: "",
  neutral: "",
};

// PipelineRow is one pipeline as a dense horizontal row inside a flow
// track: rail · identity · stages · metrics · action. Replaces the
// grid card in the dependency-grouped listing. Click the name to open
// the same overview sheet the card used.
export function PipelineRow({
  projectSlug,
  pipeline,
  edges,
  runs,
  showRail,
}: Props) {
  const run = pipeline.latest_run;
  const meta = pipeline.latest_run_meta;
  const metrics = pipeline.metrics;
  const columns = useMemo(() => buildColumns(pipeline), [pipeline]);
  const bottleneck = useMemo(() => pickBottleneck(columns), [columns]);
  const [overviewOpen, setOverviewOpen] = useState(false);

  const tone: StatusTone = run ? statusTone(run.status) : "neutral";
  const shortSha = meta?.revision ? meta.revision.slice(0, 7) : null;
  const subject = meta?.message ? truncate(firstLine(meta.message), 32) : null;
  const rate =
    metrics && metrics.runs_considered > 0
      ? Math.round(metrics.success_rate * 100)
      : null;

  // PR badge wins over the C/A badge — it's the more actionable context
  // when present (the run came from a pull request).
  const prNumber =
    meta?.cause === "pull_request" && meta?.pr_number ? meta.pr_number : null;

  return (
    <>
      <div
        id={`pl-row-${pipeline.name}`}
        className={cn(
          "grid min-h-[84px] scroll-mt-24 items-stretch gap-0 transition-colors hover:bg-muted/40",
          // Cols 4 (metrics) + 5 (action) are FIXED width so the flexible
          // identity/stage columns resolve to the same widths in every row —
          // otherwise per-row metric/action content made the stage track
          // start at a different x and the job circles didn't line up.
          "grid-cols-[46px_minmax(200px,1.15fr)_minmax(280px,1.5fr)_260px_170px]",
          rowAccent[tone],
        )}
      >
        {/* Col 1 — rail node only. The connecting line + arrow lives on the
            edge connector between rows (the "passes <artifact>" row), so the
            chain reads as a hop across the artifact, not one heavy line. */}
        <div className="flex items-center justify-center py-0">
          {showRail ? (
            <span
              className={cn(
                "size-[11px] shrink-0 rounded-full border-2 bg-background",
                nodeBorder[tone],
              )}
              aria-hidden
            />
          ) : null}
        </div>

        {/* Col 2 — identity */}
        <div className="flex min-w-0 flex-col justify-center py-3 pr-3">
          {/* flex-wrap so the badge cluster (compliance / C·A / branch / cause)
              wraps to a second line at narrow (laptop) widths instead of
              crushing or covering the pipeline name (#140). The name keeps a
              readable min width and only truncates within it. */}
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <Tooltip>
              <TooltipTrigger
                render={
                  <button
                    type="button"
                    onClick={() => setOverviewOpen(true)}
                    className="min-w-[9rem] flex-1 truncate rounded-sm text-left text-[15px] font-semibold tracking-[-0.2px] outline-none hover:underline focus-visible:ring-2 focus-visible:ring-ring"
                  />
                }
              >
                {pipeline.name}
              </TooltipTrigger>
              <TooltipContent align="start">Open pipeline overview</TooltipContent>
            </Tooltip>
            {isCompliancePipeline(pipeline.name) ? (
              <Tooltip>
                <TooltipTrigger
                  render={
                    <span className="inline-flex shrink-0 cursor-help items-center gap-1 rounded border border-border bg-muted px-1.5 py-0.5 text-[9.5px] font-semibold text-muted-foreground" />
                  }
                >
                  <ShieldCheck className="size-3" aria-hidden />
                  compliance
                </TooltipTrigger>
                <TooltipContent>
                  Server-managed compliance pipeline — runs enforced policy jobs
                </TooltipContent>
              </Tooltip>
            ) : null}
            {/* Ref badge: the run's source — a PR when triggered by one,
                otherwise the branch. Mirrors the old card's ref pill. */}
            {prNumber ? (
              <Tooltip>
                <TooltipTrigger
                  render={
                    <span className="shrink-0 cursor-help rounded border border-sky-500/30 bg-sky-500/10 px-1.5 py-0.5 font-mono text-[9.5px] font-semibold text-sky-600 dark:text-sky-400" />
                  }
                >
                  ⑂ PR #{prNumber}
                </TooltipTrigger>
                <TooltipContent>
                  Pull request #{prNumber}
                  {meta?.branch ? ` (${meta.branch})` : ""}
                </TooltipContent>
              </Tooltip>
            ) : meta?.branch ? (
              <Tooltip>
                <TooltipTrigger
                  render={
                    <span className="inline-flex max-w-[160px] shrink-0 cursor-help items-center gap-1 rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-[9.5px] text-muted-foreground" />
                  }
                >
                  <GitBranch className="size-3 shrink-0" aria-hidden />
                  <span className="truncate">{meta.branch}</span>
                </TooltipTrigger>
                <TooltipContent>Ref: {meta.branch}</TooltipContent>
              </Tooltip>
            ) : null}
            {/* Cause glyph for triggers the ref pill doesn't already convey.
                Driven by run.cause (always present) — NOT meta.cause, which is
                absent for manual/no-git runs (revisions:{}), so a manual-only
                pipeline would otherwise show no cause. Suppress push/webhook
                (the branch pill implies it) and pull_request only when the PR
                pill is actually rendered. */}
            {run?.cause &&
            !["push", "webhook"].includes(run.cause) &&
            !(run.cause === "pull_request" && prNumber) ? (
              <CauseBadge
                cause={run.cause}
                className="shrink-0 gap-1 px-1.5 py-0.5 text-[9.5px]"
              />
            ) : null}
            {/* Separate warning badge for a low change-approval rate. */}
            {rate != null && rate < 70 ? (
              <Tooltip>
                <TooltipTrigger
                  render={
                    <span className="shrink-0 cursor-help rounded border border-amber-500/30 bg-amber-500/10 px-1.5 py-0.5 font-mono text-[9.5px] font-semibold text-amber-600 dark:text-amber-400" />
                  }
                >
                  ⚠ {rate}% C/A
                </TooltipTrigger>
                <TooltipContent>
                  {rate}% change approval rate over the last{" "}
                  {metrics?.runs_considered ?? 0} runs
                </TooltipContent>
              </Tooltip>
            ) : null}
            {bottleneck ? <BottleneckPill bottleneck={bottleneck} /> : null}
          </div>
          <div className="mt-1.5 flex flex-wrap items-baseline gap-x-2 font-mono text-[11px] text-muted-foreground">
            <span>v{pipeline.definition_version}</span>
            {run ? (
              <>
                <Link
                  href={`/runs/${run.id}` as Route}
                  className="text-foreground/90 hover:underline"
                >
                  #{run.counter}
                </Link>
                <LiveDuration
                  startedAt={run.started_at}
                  finishedAt={run.finished_at}
                  className="tabular-nums"
                />
                <span>
                  <RelativeTime at={run.started_at ?? run.created_at} />
                </span>
                {shortSha ? <span className="text-foreground/70">{shortSha}</span> : null}
                {subject ? (
                  meta?.message && meta.message !== subject ? (
                    <Tooltip>
                      <TooltipTrigger
                        render={<span className="cursor-help truncate text-muted-foreground/80" />}
                      >
                        {subject}
                      </TooltipTrigger>
                      <TooltipContent className="max-w-md whitespace-pre-wrap">
                        {meta.message}
                      </TooltipContent>
                    </Tooltip>
                  ) : (
                    <span className="truncate text-muted-foreground/80">{subject}</span>
                  )
                ) : null}
              </>
            ) : (
              <span className="italic">Never run</span>
            )}
          </div>
        </div>

        {/* Col 3 — stage mini-track (one status circle per job). When the
            run declares services, the cluster leads the track INLINE (only
            then — no reserved lane), mirroring the design handoff: services
            boot before the jobs, so the violet cluster + connector sit before
            the first circle. Rows without services start at the track's left
            edge (no empty gap). */}
        <div className="flex items-center overflow-x-auto py-3 pr-3">
          {run?.has_services ? (
            <ServicesCluster names={run.service_names ?? []} tone={tone} />
          ) : null}
          <RowStages columns={columns} runId={run?.id} />
        </div>

        {/* Col 4 — metrics */}
        <div className="flex items-center gap-5 px-3 py-3">
          <Metric
            label="Lead"
            value={metrics ? formatDurationSeconds(metrics.lead_time_p50_seconds) : null}
            hint="Lead time (p50) — first commit to run finished"
          />
          <Metric
            label="Proc"
            value={metrics ? formatDurationSeconds(metrics.process_time_p50_seconds) : null}
            hint="Process time (p50) — time actually running"
          />
          <Metric
            label="C/A"
            value={rate != null ? `${rate}%` : null}
            warn={rate != null && rate < 70}
            hint={`Change approval rate over the last ${metrics?.runs_considered ?? 0} runs`}
          />
        </div>

        {/* Col 5 — action */}
        <div className="flex flex-col items-end justify-center gap-2 py-3 pl-2 pr-5">
          <TriggerPipelineButton
            pipelineId={pipeline.id}
            pipelineName={pipeline.name}
            projectSlug={projectSlug}
            currentStatus={run?.status}
          />
          <span className="font-mono text-[11px] text-muted-foreground">
            {metrics && metrics.runs_considered > 0
              ? `${metrics.runs_considered} runs · ${metrics.window_days}d`
              : "0 runs"}
          </span>
        </div>
      </div>

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

// RowStages is the compact horizontal stage track: one status box per JOB
// (so multi-job stages keep per-job action menus), grouped under the stage
// name, dashed connectors between stages, and a small C/A badge on stages
// whose recent pass rate is low.
function RowStages({
  columns,
  runId,
}: {
  columns: StageColumn[];
  runId?: string;
}) {
  if (columns.length === 0) {
    return <span className="font-mono text-[11px] text-muted-foreground">—</span>;
  }
  return (
    <div className="flex items-start">
      {columns.map((col, i) => {
        // Soft per-stage signal — ≥1 run (not ≥3) so it shows on young
        // pipelines; the hard alert is the row C/A + attention strip.
        const caLow =
          col.stat != null &&
          col.stat.runs_considered >= 1 &&
          col.stat.success_rate < 0.7;
        return (
          <div key={col.name} className="flex items-start">
            <div className="flex flex-col items-center gap-1.5">
              <div className="relative flex items-center gap-1">
                {col.jobs.length > 0 ? (
                  col.jobs.map((job) => (
                    <StageBox
                      key={job.key}
                      tone={job.run ? statusTone(job.run.status) : "neutral"}
                      tooltip={`${col.name}:${job.name} · ${job.run?.status ?? "not run"}`}
                      job={runId != null && job.run ? job : undefined}
                      runId={runId}
                    />
                  ))
                ) : (
                  <StageBox
                    tone={col.run ? statusTone(col.run.status) : "neutral"}
                    tooltip={stageTooltip(col)}
                  />
                )}
                {caLow ? (
                  <span className="absolute -right-2 -top-1.5 rounded-full bg-red-500 px-1 font-mono text-[8.5px] font-semibold text-white">
                    {Math.round((col.stat?.success_rate ?? 0) * 100)}%
                  </span>
                ) : null}
              </div>
              <span className="max-w-[80px] truncate font-mono text-[8.5px] font-semibold uppercase text-muted-foreground">
                {col.name}
              </span>
            </div>
            {i < columns.length - 1 ? (
              <span className="mx-1 mt-[14px] w-5 border-t-[1.5px] border-dashed border-border" aria-hidden />
            ) : null}
          </div>
        );
      })}
    </div>
  );
}

// StageBox is one 30px status box. With a live job + runId it becomes the
// job-action dropdown trigger (View status / Restart / Approve / Reject /
// Cancel); otherwise it's a hover tooltip.
function StageBox({
  tone,
  tooltip,
  job,
  runId,
}: {
  tone: StatusTone;
  tooltip: string;
  job?: MergedJob;
  runId?: string;
}) {
  const box = (
    <span
      className={cn(
        "flex size-[30px] items-center justify-center rounded-full border-[1.5px]",
        nodeBorder[tone],
        boxBg[tone],
        tone === "running" && "animate-pulse",
      )}
    >
      <StageGlyph tone={tone} />
    </span>
  );
  if (job && runId) {
    return (
      <JobActions job={job} runId={runId} tooltip={tooltip} triggerClassName="rounded-full">
        {box}
      </JobActions>
    );
  }
  return (
    <Tooltip>
      <TooltipTrigger render={<span className="inline-flex cursor-default" />}>
        {box}
      </TooltipTrigger>
      <TooltipContent>{tooltip}</TooltipContent>
    </Tooltip>
  );
}

const boxBg: Record<StatusTone, string> = {
  success: "bg-emerald-500/15",
  failed: "bg-red-500/15",
  running: "bg-sky-500/15",
  queued: "bg-muted",
  warning: "bg-amber-500/10",
  awaiting: "bg-muted",
  canceled: "bg-muted",
  skipped: "bg-muted",
  neutral: "bg-muted",
};

function StageGlyph({ tone }: { tone: StatusTone }) {
  if (tone === "success") return <Check className="size-[15px] text-emerald-500" aria-hidden />;
  if (tone === "failed") return <X className="size-[15px] text-red-500" aria-hidden />;
  if (tone === "running")
    return <Loader2 className="size-[15px] animate-spin text-sky-500" aria-hidden />;
  if (tone === "queued" || tone === "awaiting")
    return <span className="font-mono text-[13px] text-muted-foreground">»</span>;
  return <Minus className="size-[15px] text-muted-foreground" aria-hidden />;
}

function Metric({
  label,
  value,
  warn,
  hint,
}: {
  label: string;
  value: string | null;
  warn?: boolean;
  hint?: string;
}) {
  const inner = (
    <>
      <span className="font-mono text-[9px] uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      <span
        className={cn(
          "font-mono text-[13px] font-semibold tabular-nums",
          value == null
            ? "text-muted-foreground"
            : warn
              ? "text-red-500"
              : "text-foreground",
        )}
      >
        {value ?? "—"}
      </span>
    </>
  );
  if (!hint) return <div className="flex flex-col gap-0.5">{inner}</div>;
  return (
    <Tooltip>
      <TooltipTrigger render={<div className="flex cursor-help flex-col gap-0.5" />}>
        {inner}
      </TooltipTrigger>
      <TooltipContent>{hint}</TooltipContent>
    </Tooltip>
  );
}

// stageTooltip mirrors the old per-job hover: stage · status · duration ·
// historical C/A — the at-a-glance peek without opening anything.
function stageTooltip(col: StageColumn): string {
  const parts: string[] = [col.name, col.run?.status ?? "not run"];
  if (col.durationSec != null) parts.push(formatDurationSeconds(col.durationSec));
  if (col.stat && col.stat.runs_considered > 0) {
    parts.push(`${Math.round(col.stat.success_rate * 100)}% C/A`);
  }
  return parts.join(" · ");
}

// BottleneckPill surfaces the pipeline's worst stage (slowest vs its own
// p50, or lowest pass rate) — the header callout the old card carried.
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
          <span className="inline-flex shrink-0 cursor-help items-center gap-1 rounded-full border border-amber-500/30 bg-amber-500/10 px-2 py-0.5 text-[10px] text-amber-600 dark:text-amber-400" />
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

function truncate(s: string, maxLen: number): string {
  return s.length <= maxLen ? s : s.slice(0, maxLen).trimEnd() + "…";
}

function firstLine(message: string): string {
  const idx = message.indexOf("\n");
  return idx >= 0 ? message.slice(0, idx) : message;
}
