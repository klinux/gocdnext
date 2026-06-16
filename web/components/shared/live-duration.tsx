"use client";

import { useEffect, useState } from "react";

import { durationBetween, formatDurationSeconds } from "@/lib/format";

type Props = {
  startedAt?: string | null;
  finishedAt?: string | null;
  fallback?: string;
  className?: string;
};

// LiveDuration renders "Xs/m/h" for a started interval. The trick
// is that durations of running runs compute against `Date.now()`,
// which differs between the SSR pass and the client's first
// paint — without care, hydration produces a "23s vs 24s" mismatch
// on every F5 mid-run. Strategy:
//   - compute from props during render so startedAt/finishedAt
//     changes show immediately;
//   - suppress hydration warning on the text element only, because
//     time can legitimately differ by a second between SSR and the
//     client's first paint;
//   - while `finishedAt` is missing, tick every second so the label
//     stays live without needing a server refresh.
export function LiveDuration({
  startedAt,
  finishedAt,
  fallback = "—",
  className,
}: Props) {
  const [, bump] = useState(0);

  useEffect(() => {
    if (!startedAt) return;
    if (finishedAt) return; // terminal → no ticking needed
    const id = setInterval(() => bump((n) => n + 1), 1000);
    return () => clearInterval(id);
  }, [startedAt, finishedAt]);

  if (!startedAt) return <span className={className}>{fallback}</span>;
  const label = formatDurationSeconds(durationBetween(startedAt, finishedAt));

  return (
    <span className={className} suppressHydrationWarning>
      {label}
    </span>
  );
}
