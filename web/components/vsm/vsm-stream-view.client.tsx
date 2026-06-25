"use client";

import { AlertTriangle, Check, GitCommitHorizontal } from "lucide-react";

import { cn } from "@/lib/utils";
import { formatDurationSeconds } from "@/lib/format";
import { statusTone, type StatusTone } from "@/lib/status";
import {
  buildVSMStreams,
  type ProjectRollup,
  type Stream,
  type StreamStep,
} from "@/lib/vsm-stream";
import type { ProjectVSM, VSMNode } from "@/types/api";

// Lead-label column wide enough to clear the spine bar (which sits at the
// right edge of this column, touching the card) — see StepRow.
const SPINE = "grid grid-cols-[130px_1fr]";

// VSMStreamView is the vertical value-stream map: each dependency chain
// (path to production) becomes a top-to-bottom timeline with a lead-time
// spine on the left, full-width process cards, handoff/gate edges, and a
// summary of the path's lead vs process time + rolled yield. Pipelines off
// the path render in a separate "outside the stream" section.
export function VSMStreamView({ vsm }: { vsm: ProjectVSM }) {
  const { streams, outside, rollup } = buildVSMStreams(vsm);

  return (
    <div className="space-y-8 p-5">
      {streams.map((stream) => (
        <StreamView key={stream.path} stream={stream} />
      ))}
      {outside.length > 0 ? <OutsideSection nodes={outside} /> : null}
      <ProjectRollupStrip rollup={rollup} />
      <Legend />
    </div>
  );
}

// ProjectRollupStrip is the project-wide DORA summary (runs-weighted) — the
// panel the old VSM metrics strip carried, kept so the whole-project view
// survives the move to per-stream summaries.
function ProjectRollupStrip({ rollup }: { rollup: ProjectRollup }) {
  const success = rollup.successAvg != null ? Math.round(rollup.successAvg * 100) : null;
  const rolled = rollup.worstRolledCA != null ? Math.round(rollup.worstRolledCA * 100) : null;
  return (
    <section className="space-y-2.5">
      <div className="flex items-center gap-3 px-1">
        <span className="font-mono text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
          Project rollup
        </span>
        <span className="h-px flex-1 bg-border" aria-hidden />
      </div>
      <div className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-border bg-border md:grid-cols-5">
        <Tile
          label="Lead P50 avg"
          value={formatDurationSeconds(rollup.leadP50AvgSec)}
          sub="weighted by runs"
        />
        <Tile
          label="Process P50 avg"
          value={formatDurationSeconds(rollup.processP50AvgSec)}
          sub="weighted by runs"
        />
        <Tile
          label="Success avg"
          value={success != null ? `${success}%` : "—"}
          sub={`${rollup.runsTotal} runs · ${rollup.windowDays}d`}
          tone={success == null ? undefined : success >= 90 ? "good" : success >= 70 ? "warn" : "bad"}
        />
        <Tile label="Pipelines" value={String(rollup.pipelineCount)} sub="in project" />
        <Tile
          label="Rolled %C/A"
          value={rolled != null ? `${rolled}%` : "—"}
          sub="worst path"
          tone={rolled == null ? undefined : rolled >= 90 ? "good" : "bad"}
        />
      </div>
    </section>
  );
}

function StreamView({ stream }: { stream: Stream }) {
  return (
    <section>
      <header className="mb-3 flex flex-wrap items-center gap-2 px-1">
        <span className="text-[15px] font-semibold">Path to production</span>
        <span className="font-mono text-xs text-muted-foreground">
          commit → {stream.path} → prod
        </span>
        {stream.bottleneckName ? (
          <span className="ml-auto inline-flex items-center gap-1 rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-[11px] font-semibold text-amber-600 dark:text-amber-400">
            <AlertTriangle className="size-3" aria-hidden />
            bottleneck: {stream.bottleneckName}
          </span>
        ) : null}
      </header>

      <div className="rounded-2xl border border-border bg-card p-4">
        <Endpoint kind="src" />
        {stream.steps.map((step, i) => (
          <div key={step.node.pipeline_id}>
            {i > 0 ? (
              <HandoffEdge
                artifact={step.artifactIn}
                waitSec={step.waitInSec}
                blocked={isIdle(step.status)}
              />
            ) : null}
            <StepRow step={step} />
          </div>
        ))}
        <Endpoint kind="dst" totalSec={stream.leadTotalSec} />
      </div>

      <Summary stream={stream} />
    </section>
  );
}

// StepRow is one pipeline: the spine cell (lead label · bar · node) beside
// the process card.
function StepRow({ step }: { step: StreamStep }) {
  const tone: StatusTone = step.status ? statusTone(step.status) : "neutral";
  return (
    <div className={SPINE}>
      <div className="relative">
        {/* lead accrued — owns the left of the column, clear of the bar */}
        <div className="absolute left-0 top-2.5 w-[92px] text-right">
          <div className="font-mono text-xs font-bold tabular-nums">
            {formatDurationSeconds(step.cumulativeLeadSec)}
          </div>
          <div className="font-mono text-[9px] text-muted-foreground">
            since commit
          </div>
        </div>
        {/* bar runs full-height at the right edge, touching the card */}
        <span
          className={cn(
            "absolute inset-y-0 left-[109px] w-1.5",
            step.bottleneck ? "bg-amber-500/70" : "bg-primary/40",
          )}
          aria-hidden
        />
        {/* node circle pinned to the top of the card, centred on the bar */}
        <span
          className={cn(
            "absolute left-[105px] top-4 size-[15px] rounded-full border-[2.5px] bg-background ring-4 ring-card",
            nodeBorder[tone],
          )}
          aria-hidden
        />
      </div>
      <ProcessCard step={step} tone={tone} />
    </div>
  );
}

function ProcessCard({ step, tone }: { step: StreamStep; tone: StatusTone }) {
  const idle = isIdle(step.status);
  return (
    <div
      className={cn(
        "my-2 rounded-xl border bg-muted/30 px-4 py-3",
        step.bottleneck
          ? "border-amber-500/50 ring-2 ring-amber-500/20"
          : "border-border",
        idle && "border-dashed bg-transparent",
      )}
    >
      <div className="flex items-center gap-2">
        <span className={cn("size-2.5 shrink-0 rounded-full", dotBg[tone])} aria-hidden />
        <span className="font-mono text-sm font-semibold">{step.node.name}</span>
        {step.bottleneck ? (
          <span className="inline-flex items-center gap-1 rounded border border-amber-500/40 bg-amber-500/10 px-1.5 py-0.5 text-[9.5px] font-semibold text-amber-600 dark:text-amber-400">
            <AlertTriangle className="size-3" aria-hidden />
            BOTTLENECK
          </span>
        ) : null}
        <div className="ml-auto flex items-center gap-5">
          <Metric label="PT" value={formatDurationSeconds(step.processSec)} />
          <Metric
            label="Throughput"
            value={step.throughputPerDay != null ? `${step.throughputPerDay.toFixed(1)}/d` : "—"}
          />
        </div>
      </div>

      {idle ? (
        <p className="mt-2 font-mono text-[11px] italic text-muted-foreground">
          Waiting on gate — blocked upstream.
        </p>
      ) : (
        <CABar rate={step.caRate} />
      )}
    </div>
  );
}

// CABar is the change-approval yield track with a goal tick at 90%.
function CABar({ rate }: { rate: number | null }) {
  const pct = rate != null ? Math.round(rate * 100) : null;
  return (
    <div className="mt-3 flex items-center gap-2">
      <span className="font-mono text-[9px] uppercase tracking-wide text-muted-foreground">
        C/A
      </span>
      <div className="relative h-[7px] flex-1 overflow-hidden rounded-full bg-muted">
        {pct != null ? (
          <span
            className={cn("absolute inset-y-0 left-0 rounded-full", caFill(pct))}
            style={{ width: `${pct}%` }}
            aria-hidden
          />
        ) : null}
        {/* goal tick at 90% */}
        <span className="absolute inset-y-0 left-[90%] w-px bg-muted-foreground/50" aria-hidden />
      </div>
      <span
        className={cn(
          "w-9 text-right font-mono text-xs font-semibold tabular-nums",
          pct == null ? "text-muted-foreground" : caText(pct),
        )}
      >
        {pct != null ? `${pct}%` : "—"}
      </span>
    </div>
  );
}

// HandoffEdge is the gap between two steps: a thin wait bar on the spine +
// the artifact handed off and how long it waited (or a manual gate).
function HandoffEdge({
  artifact,
  waitSec,
  blocked,
}: {
  artifact: string | null;
  waitSec: number;
  blocked: boolean;
}) {
  return (
    <div className={cn(SPINE, "min-h-[34px]")}>
      <div className="relative">
        <span className="absolute inset-y-0 left-[111px] w-0.5 bg-muted-foreground/40" aria-hidden />
      </div>
      <div className="flex items-center gap-2 py-2 text-[11px] text-muted-foreground">
        {blocked ? (
          <span className="inline-flex items-center gap-1 rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 font-mono font-semibold text-amber-600 dark:text-amber-400">
            ⏸ manual gate
          </span>
        ) : null}
        {artifact ? (
          <span className="rounded-full border border-primary/35 bg-primary/10 px-2 py-0.5 font-mono font-semibold text-primary">
            {artifact}
          </span>
        ) : null}
        <span className="font-mono">
          {blocked ? "waiting" : "handoff"} · {formatDurationSeconds(waitSec)}
        </span>
      </div>
    </div>
  );
}

function Endpoint({ kind, totalSec }: { kind: "src" | "dst"; totalSec?: number }) {
  return (
    <div className={SPINE}>
      <div className="relative">
        <span
          className={cn(
            "absolute left-[104px] flex size-4 items-center justify-center rounded-full border-2 bg-background ring-4 ring-card",
            kind === "src" ? "border-primary" : "border-emerald-500",
            kind === "src" ? "top-0" : "bottom-0",
          )}
          aria-hidden
        >
          {kind === "src" ? (
            <GitCommitHorizontal className="size-3 text-primary" aria-hidden />
          ) : (
            <Check className="size-3 text-emerald-500" aria-hidden />
          )}
        </span>
      </div>
      <div className={cn("font-mono text-[11px] text-muted-foreground", kind === "src" ? "pb-3" : "pt-3")}>
        {kind === "src" ? (
          <>COMMIT · push</>
        ) : (
          <>
            PRODUCTION
            {totalSec != null ? ` · ~${formatDurationSeconds(totalSec)} total` : ""}
          </>
        )}
      </div>
    </div>
  );
}

function Summary({ stream }: { stream: Stream }) {
  const eff = stream.flowEfficiency != null ? Math.round(stream.flowEfficiency * 100) : null;
  const rolled = stream.rolledCA != null ? Math.round(stream.rolledCA * 100) : null;
  return (
    <div className="mt-3 grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-border bg-border md:grid-cols-4">
      <Tile label="Lead time total" value={formatDurationSeconds(stream.leadTotalSec)} sub="commit → prod" />
      <Tile label="Σ process time" value={formatDurationSeconds(stream.processTotalSec)} sub="real work" />
      <Tile
        label="Flow efficiency"
        value={eff != null ? `${eff}%` : "—"}
        sub="process ÷ lead"
        tone={eff != null && eff >= 40 ? "good" : "warn"}
      />
      <Tile
        label="Rolled %C/A"
        value={rolled != null ? `${rolled}%` : "—"}
        sub="chain yield"
        tone={rolled != null && rolled >= 90 ? "good" : "bad"}
      />
    </div>
  );
}

function Tile({
  label,
  value,
  sub,
  tone,
}: {
  label: string;
  value: string;
  sub: string;
  tone?: "good" | "warn" | "bad";
}) {
  return (
    <div className="bg-card px-4 py-3">
      <div className="font-mono text-[9.5px] uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      <div
        className={cn(
          "mt-1 font-mono text-2xl font-bold tabular-nums",
          tone === "good" && "text-emerald-500",
          tone === "warn" && "text-amber-500",
          tone === "bad" && "text-red-500",
        )}
      >
        {value}
      </div>
      <div className="font-mono text-[11px] text-muted-foreground">{sub}</div>
    </div>
  );
}

function OutsideSection({ nodes }: { nodes: VSMNode[] }) {
  return (
    <section className="space-y-2.5">
      <div className="flex items-center gap-3 px-1">
        <span className="font-mono text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
          Outside the path to production
        </span>
        <span className="h-px flex-1 bg-border" aria-hidden />
      </div>
      <div className="grid gap-3 md:grid-cols-2">
        {nodes.map((n) => {
          const tone: StatusTone = n.latest_run ? statusTone(n.latest_run.status) : "neutral";
          const m = n.metrics;
          const rate = m && m.runs_considered > 0 ? Math.round(m.success_rate * 100) : null;
          const tput = m && m.window_days > 0 ? m.runs_considered / m.window_days : null;
          return (
            <div
              key={n.pipeline_id}
              className="flex items-center gap-3 rounded-xl border border-border bg-card px-4 py-3"
            >
              <span className={cn("size-2.5 shrink-0 rounded-full", dotBg[tone])} aria-hidden />
              <span className="font-mono text-sm font-semibold">{n.name}</span>
              <div className="ml-auto flex items-center gap-4">
                <Metric label="C/A" value={rate != null ? `${rate}%` : "—"} />
                <Metric
                  label="PT"
                  value={m ? formatDurationSeconds(m.process_time_p50_seconds) : "—"}
                />
                <Metric
                  label="Throughput"
                  value={tput != null ? `${tput.toFixed(1)}/d` : "—"}
                />
              </div>
            </div>
          );
        })}
      </div>
    </section>
  );
}

function Legend() {
  return (
    <div className="flex flex-wrap items-center gap-6 border-t border-border pt-3 font-mono text-[11px] text-muted-foreground">
      <span className="inline-flex items-center gap-2">
        <span className="h-3 w-1.5 bg-primary/40" aria-hidden />
        processing
      </span>
      <span className="inline-flex items-center gap-2">
        <span className="h-3 w-1.5 bg-amber-500/70" aria-hidden />
        bottleneck
      </span>
      <span className="inline-flex items-center gap-2">
        <span className="h-3 w-0.5 bg-muted-foreground/40" aria-hidden />
        wait / gate
      </span>
      <span>node = pipeline · number = lead accrued</span>
    </div>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col">
      <span className="font-mono text-[9px] uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      <span className="font-mono text-[13px] font-semibold tabular-nums">{value}</span>
    </div>
  );
}

function isIdle(status: string | undefined): boolean {
  return (
    status == null ||
    status === "awaiting_approval" ||
    status === "waiting" ||
    status === "queued"
  );
}

function caFill(pct: number): string {
  if (pct >= 90) return "bg-emerald-500";
  if (pct >= 50) return "bg-amber-500";
  return "bg-red-500";
}

function caText(pct: number): string {
  if (pct >= 90) return "text-emerald-500";
  if (pct >= 50) return "text-amber-500";
  return "text-red-500";
}

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

const dotBg: Record<StatusTone, string> = {
  success: "bg-emerald-500",
  failed: "bg-red-500",
  running: "bg-sky-500",
  queued: "bg-amber-500",
  warning: "bg-amber-500",
  awaiting: "bg-amber-500",
  canceled: "bg-muted-foreground/60",
  skipped: "bg-muted-foreground/40",
  neutral: "bg-muted-foreground/40",
};
