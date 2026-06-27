"use client";

import { useEffect, useId, useRef, useState } from "react";
import { ChevronDown } from "lucide-react";

import { cn } from "@/lib/utils";
import { formatDurationSeconds } from "@/lib/format";
import { DurationSparkline } from "./duration-sparkline";
import { durationSummary, type DurationPoint } from "./duration-trend";

// Bar tint by duration relative to the median (not run status — this view is
// duration-only). Mirrors the sparkline's regression thresholds.
function barClass(v: number, median: number): string {
  if (v > median * 1.35) return "bg-red-500 opacity-100";
  if (v > median * 1.15) return "bg-amber-500 opacity-85";
  return "bg-emerald-500 opacity-85";
}

// DurationTrendPill is a compact toolbar control: label + sparkline + median +
// a window-over-window delta badge, expanding to a full per-run histogram on
// click. Zero vertical footprint until opened. Closes on Escape / outside
// click. Renders nothing below 2 finished runs.
export function DurationTrendPill({
  points,
  label = "DURATION",
  note,
  className,
}: {
  points: DurationPoint[];
  label?: string;
  note?: string;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const popId = useId();

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const summary = durationSummary(points);
  if (!summary) return null;
  const { median, min, max, deltaPct, slower } = summary;

  return (
    <div ref={rootRef} className={cn("relative", className)}>
      <div
        role="button"
        tabIndex={0}
        aria-expanded={open}
        aria-controls={popId}
        aria-label={`Run duration trend, median ${formatDurationSeconds(median)}`}
        onClick={() => setOpen((o) => !o)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            setOpen((o) => !o);
          }
        }}
        className={cn(
          "inline-flex h-[34px] cursor-pointer items-center gap-2.5 rounded-[11px] border bg-card px-3 transition-colors",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          open ? "border-primary" : "border-border hover:border-muted-foreground/40 hover:bg-accent",
        )}
      >
        <span className="hidden font-mono text-[10px] font-semibold uppercase tracking-wide text-muted-foreground lg:inline">
          {label}
        </span>
        <DurationSparkline values={summary.values} />
        <span className="font-mono text-[12.5px] font-semibold text-foreground">
          {formatDurationSeconds(median)}
        </span>
        {deltaPct !== null && deltaPct !== 0 ? (
          <span
            className={cn(
              "hidden rounded-full px-1.5 py-0.5 text-[11px] font-semibold leading-none sm:inline",
              slower ? "bg-red-500/[0.13] text-red-400" : "bg-emerald-500/[0.13] text-emerald-400",
            )}
          >
            {slower ? "↑" : "↓"} {Math.abs(deltaPct)}%
          </span>
        ) : null}
        <ChevronDown
          aria-hidden
          className={cn(
            "size-3.5 text-muted-foreground transition-transform",
            open && "rotate-180",
          )}
        />
      </div>

      {open ? (
        <div
          id={popId}
          className="absolute right-0 top-[calc(100%+8px)] z-20 w-[460px] max-w-[calc(100vw-2rem)] rounded-[14px] border border-border bg-popover p-4 shadow-2xl"
        >
          <div className="mb-3 flex items-center justify-between">
            <span className="font-mono text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
              Run duration · {summary.values.length} runs
            </span>
            {note ? <span className="text-[11px] text-muted-foreground">{note}</span> : null}
          </div>

          <div className="flex h-[60px] items-end gap-[3px]">
            {points
              .filter((p) => p.durationSeconds > 0)
              .map((p, i) => {
                const h = 8 + ((p.durationSeconds - min) / (max - min || 1)) * 92;
                return (
                  <div
                    key={`${p.label}-${i}`}
                    title={`${p.label} · ${formatDurationSeconds(p.durationSeconds)}`}
                    className={cn(
                      "min-h-[3px] flex-1 rounded-t-sm transition-opacity hover:opacity-100",
                      barClass(p.durationSeconds, median),
                    )}
                    style={{ height: `${h}%` }}
                  />
                );
              })}
          </div>

          <div className="mt-2.5 flex justify-between font-mono text-[10.5px] text-muted-foreground">
            <span>fastest {formatDurationSeconds(min)}</span>
            <span className="text-foreground">median {formatDurationSeconds(median)}</span>
            <span>slowest {formatDurationSeconds(max)}</span>
          </div>
        </div>
      ) : null}
    </div>
  );
}
