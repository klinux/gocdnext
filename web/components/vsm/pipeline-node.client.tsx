"use client";

import { forwardRef } from "react";
import Link from "next/link";
import type { Route } from "next";
import { AlertTriangle } from "lucide-react";

import type { StageStat, VSMNode as VSMNodeT } from "@/types/api";
import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import { formatDurationSeconds } from "@/lib/format";

type Props = {
  pipeline: VSMNodeT;
  bottleneck?: boolean;
};

// PipelineNode is the compact VSM card. Deliberately minimal —
// name + the two headline stats (process-time p50, pass rate · runs
// per day) + the stage health bar at the foot. Git ref and run
// counter moved out of the VSM (they belong on the Pipelines tab
// sheet); the VSM is a macro view of the value stream, not a
// detail drawer. Exposes forwardRef so the canvas can measure the
// card to draw SVG arrows between siblings.
export const PipelineNode = forwardRef<HTMLDivElement, Props>(
  function PipelineNode({ pipeline, bottleneck }, ref) {
    const latest = pipeline.latest_run;
    const metrics = pipeline.metrics;
    const tone: StatusTone = latest ? statusTone(latest.status) : "neutral";

    const hasStats = metrics != null && metrics.runs_considered > 0;
    const processTime = hasStats
      ? formatDurationSeconds(metrics!.process_time_p50_seconds)
      : null;
    const passRate = hasStats
      ? Math.round(metrics!.success_rate * 100)
      : null;
    const runsPerDay =
      hasStats && metrics!.window_days > 0
        ? (metrics!.runs_considered / metrics!.window_days).toFixed(1)
        : null;

    return (
      <div
        ref={ref}
        className={cn(
          "w-[220px] rounded-md border bg-card p-2.5 shadow-sm",
          bottleneck
            ? "border-amber-500/60 ring-2 ring-amber-500/25"
            : "border-border",
        )}
      >
        <div className="flex items-center gap-1.5">
          <span
            className={cn(
              "inline-flex size-3 shrink-0 items-center justify-center rounded-full",
              toneDotClasses[tone],
              latest?.status === "running" && "animate-pulse",
            )}
            aria-hidden
            title={latest?.status ?? "not run"}
          />
          {latest ? (
            <Link
              href={`/runs/${latest.id}` as Route}
              className="min-w-0 flex-1 truncate font-mono text-sm font-semibold hover:underline"
              title={`Open latest run of ${pipeline.name}`}
            >
              {pipeline.name}
            </Link>
          ) : (
            <span className="min-w-0 flex-1 truncate font-mono text-sm font-semibold">
              {pipeline.name}
            </span>
          )}
        </div>

        {hasStats ? (
          <div className="mt-1.5 space-y-0.5 font-mono text-[10px] tabular-nums text-muted-foreground">
            <div>
              <span className="text-muted-foreground/70">PT</span>{" "}
              <span className="text-foreground">{processTime}</span>
            </div>
            <div>
              <span className="text-muted-foreground/70">C/A</span>{" "}
              <span
                className={cn(
                  "font-medium",
                  passRate != null && passRate >= 95
                    ? "text-emerald-500"
                    : passRate != null && passRate >= 70
                      ? "text-amber-500"
                      : "text-red-500",
                )}
              >
                {passRate}%
              </span>
              {runsPerDay ? (
                <>
                  <span className="text-muted-foreground/50"> · </span>
                  <span>{runsPerDay}/d</span>
                </>
              ) : null}
            </div>
          </div>
        ) : (
          <p className="mt-1.5 text-[10px] italic text-muted-foreground">
            {latest ? `run #${latest.counter}` : "no terminal runs yet"}
          </p>
        )}

        <StageHealthBar stats={metrics?.stage_stats} />

        {bottleneck ? (
          <div className="mt-1.5 flex items-start gap-1 rounded border border-amber-500/30 bg-amber-500/5 px-1.5 py-1 text-[10px] text-amber-700 dark:text-amber-400">
            <AlertTriangle className="mt-0.5 size-3 shrink-0" aria-hidden />
            <span>bottleneck</span>
          </div>
        ) : null}
      </div>
    );
  },
);

// StageHealthBar is the thin coloured strip at the foot of the
// node. One segment per stage, coloured by the stage's 7-day
// success rate — emerald ≥95, amber 70-95, red below. Stages with
// no terminal-run data render as a muted placeholder so the width
// stays consistent while the pipeline warms up.
function StageHealthBar({ stats }: { stats?: StageStat[] }) {
  if (!stats || stats.length === 0) {
    return (
      <div className="mt-2 h-1 rounded-sm bg-muted-foreground/15" aria-hidden />
    );
  }
  return (
    <div className="mt-2 flex h-1 gap-px" aria-hidden>
      {stats.map((s) => {
        const rate = s.success_rate;
        const hasData = s.runs_considered > 0;
        const bg = !hasData
          ? "bg-muted-foreground/20"
          : rate >= 0.95
            ? "bg-emerald-500"
            : rate >= 0.7
              ? "bg-amber-500"
              : "bg-red-500";
        return (
          <div
            key={s.name}
            className={cn("min-w-0 flex-1 rounded-sm", bg)}
            title={
              hasData
                ? `${s.name} — ${Math.round(rate * 100)}% (${s.runs_considered} runs)`
                : `${s.name} — no terminal runs`
            }
          />
        );
      })}
    </div>
  );
}

const toneDotClasses: Record<StatusTone, string> = {
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
