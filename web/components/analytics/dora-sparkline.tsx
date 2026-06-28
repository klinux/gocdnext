import { useId } from "react";

import { cn } from "@/lib/utils";

// DoraSparkline plots a daily metric series (oldest→newest) as a filled area +
// line in a single metric color — the hero-card trend. Stretches to its
// container width; the stroke stays crisp via non-scaling-stroke. Flat or
// single-point series still render a baseline so the card never shows an empty
// hole. Pure SVG, no chart lib.
export function DoraSparkline({
  values,
  color,
  height = 40,
  className,
}: {
  values: number[];
  color: string;
  height?: number;
  className?: string;
}) {
  const id = useId();
  const w = 240;
  const n = values.length;
  if (n === 0) return null;

  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = max - min || 1;
  const x = (i: number) => (n === 1 ? w / 2 : (i / (n - 1)) * w);
  const y = (v: number) => height - 3 - ((v - min) / range) * (height - 6);

  const line = values
    .map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)} ${y(v).toFixed(1)}`)
    .join(" ");
  const area = `${line} L${w} ${height} L0 ${height} Z`;
  const gid = `dora-spark-${id}`;

  return (
    <svg
      viewBox={`0 0 ${w} ${height}`}
      preserveAspectRatio="none"
      height={height}
      className={cn("block w-full", className)}
      aria-hidden
    >
      <defs>
        <linearGradient id={gid} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor={color} stopOpacity="0.24" />
          <stop offset="1" stopColor={color} stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={area} fill={`url(#${gid})`} />
      <path
        d={line}
        fill="none"
        stroke={color}
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}
