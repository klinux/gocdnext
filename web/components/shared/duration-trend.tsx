"use client";

import { useMemo } from "react";

import { cn } from "@/lib/utils";
import { formatDurationSeconds } from "@/lib/format";
import { statusTone, type StatusTone } from "@/lib/status";
import type { RunSummary } from "@/types/api";

export type DurationPoint = {
  label: string;
  durationSeconds: number;
  status: string;
};

// Bar tint per run status — saturated for the meaningful states, muted for
// the rest. Mirrors the row/node status language.
const barTone: Record<StatusTone, string> = {
  success: "bg-emerald-500/70",
  failed: "bg-red-500/70",
  canceled: "bg-red-500/40",
  running: "bg-sky-500/70",
  queued: "bg-amber-500/60",
  warning: "bg-amber-500/60",
  awaiting: "bg-amber-500/60",
  skipped: "bg-muted-foreground/30",
  neutral: "bg-muted-foreground/40",
};

// runDurationPoints turns runs into oldest→newest duration points (finished
// runs only — an in-flight run has no duration yet), capped at `limit`.
export function runDurationPoints(runs: RunSummary[], limit = 30): DurationPoint[] {
  return runs
    .filter((r) => r.started_at && r.finished_at)
    .map((r) => ({
      counter: r.counter,
      label: `#${r.counter}`,
      durationSeconds: Math.max(
        0,
        (new Date(r.finished_at!).getTime() - new Date(r.started_at!).getTime()) / 1000,
      ),
      status: r.status,
    }))
    .sort((a, b) => a.counter - b.counter)
    .slice(-limit)
    .map(({ counter: _counter, ...p }) => p);
}

// DurationTrend renders recent run durations as bars (oldest→newest) with a
// median reference line; the latest run is accented so a slowdown ("this run is
// well above median") reads at a glance. Pure presentational, no chart lib —
// matches the hand-rolled coverage trend. Returns null below 2 data points.
export function DurationTrend({
  points,
  className,
  height = 56,
}: {
  points: DurationPoint[];
  className?: string;
  height?: number;
}) {
  const stats = useMemo(() => {
    const durs = points.map((p) => p.durationSeconds).filter((d) => d > 0);
    if (durs.length < 2) return null;
    const sorted = [...durs].sort((a, b) => a - b);
    return {
      max: sorted[sorted.length - 1]!,
      median: sorted[Math.floor(sorted.length / 2)]!,
    };
  }, [points]);

  if (!stats) {
    return (
      <p className={cn("text-xs text-muted-foreground", className)}>
        Not enough finished runs yet.
      </p>
    );
  }
  const { max, median } = stats;

  return (
    <div className={className}>
      <div className="relative" style={{ height }}>
        {/* median reference line */}
        <div
          className="pointer-events-none absolute inset-x-0 border-t border-dashed border-muted-foreground/40"
          style={{ bottom: `${(median / max) * 100}%` }}
          aria-hidden
        />
        <div className="flex h-full items-end gap-0.5">
          {points.map((p, i) => {
            const last = i === points.length - 1;
            const h = p.durationSeconds > 0 ? Math.max(4, (p.durationSeconds / max) * 100) : 3;
            return (
              <div
                key={`${p.label}-${i}`}
                title={`${p.label} · ${formatDurationSeconds(p.durationSeconds)} · ${p.status}`}
                className={cn(
                  "flex-1 rounded-sm transition-colors",
                  barTone[statusTone(p.status)],
                  last && "outline outline-1 outline-offset-1 outline-primary",
                )}
                style={{ height: `${h}%` }}
              />
            );
          })}
        </div>
      </div>
      <div className="mt-1 flex justify-between font-mono text-[10px] text-muted-foreground">
        <span>{points.length} runs</span>
        <span>median {formatDurationSeconds(median)}</span>
      </div>
    </div>
  );
}
