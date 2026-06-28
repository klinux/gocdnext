import { useId } from "react";

import { cn } from "@/lib/utils";

// DurationSparkline draws a run-duration series as a compact line (oldest→
// newest). The stroke is teal while the series tracks its median and bleeds to
// amber→red from the point it first regresses past median × 1.15 — so a
// slowdown reads at a glance without a legend. A dashed reference line marks
// the median. Pure SVG, no chart lib. Returns null below 2 positive points.
export function DurationSparkline({
  values,
  width = 84,
  height = 26,
  strokeWidth = 1.8,
  fill = false,
  className,
}: {
  values: number[];
  width?: number;
  height?: number;
  strokeWidth?: number;
  // fill: stretch to the container width (preserveAspectRatio none); the stroke
  // stays crisp via non-scaling-stroke. For the wide sheet view vs the fixed
  // toolbar pill.
  fill?: boolean;
  className?: string;
}) {
  const id = useId();
  const durs = values.filter((v) => v > 0);
  if (durs.length < 2) return null;

  const min = Math.min(...durs);
  const max = Math.max(...durs);
  const range = max - min || 1;
  const n = durs.length;
  const x = (i: number) => (i / (n - 1)) * width;
  // 2px vertical padding so the stroke never clips at the extremes.
  const y = (v: number) => height - 2 - ((v - min) / range) * (height - 4);

  const sorted = [...durs].sort((a, b) => a - b);
  const median =
    sorted.length % 2
      ? sorted[(sorted.length - 1) / 2]!
      : (sorted[sorted.length / 2 - 1]! + sorted[sorted.length / 2]!) / 2;

  const d = durs.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join(" ");
  const my = y(median);

  // Gradient flips from teal at the index the series first crosses 1.15× median
  // (the regression point); clamped to [0,1]. No crossing → all teal (the
  // amber/red stops are omitted entirely so antialiasing can't paint a red
  // sliver at the tail of a healthy series).
  const regIdx = durs.findIndex((v) => v > median * 1.15);
  const regressed = regIdx >= 0;
  const stop = regressed ? Math.max(0, Math.min(1, regIdx / (n - 1))) : 1;
  const gid = `dur-spark-${id}`;

  return (
    <svg
      width={fill ? undefined : width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio={fill ? "none" : undefined}
      className={cn(fill ? "w-full" : "shrink-0", className)}
      aria-hidden
    >
      <line
        x1="0"
        y1={my.toFixed(1)}
        x2={width}
        y2={my.toFixed(1)}
        className="text-muted-foreground/40"
        stroke="currentColor"
        strokeWidth="1"
        strokeDasharray="2 2"
        vectorEffect="non-scaling-stroke"
      />
      <path
        d={d}
        fill="none"
        stroke={`url(#${gid})`}
        strokeWidth={strokeWidth}
        strokeLinecap="round"
        strokeLinejoin="round"
        vectorEffect="non-scaling-stroke"
      />
      <defs>
        <linearGradient id={gid} x1="0" y1="0" x2="1" y2="0">
          <stop offset="0" style={{ stopColor: "var(--teal)" }} />
          <stop offset={stop.toFixed(2)} style={{ stopColor: "var(--teal)" }} />
          {regressed ? (
            <>
              <stop offset={Math.min(1, stop + 0.08).toFixed(2)} style={{ stopColor: "var(--amber)" }} />
              <stop offset="1" style={{ stopColor: "var(--red)" }} />
            </>
          ) : null}
        </linearGradient>
      </defs>
    </svg>
  );
}
