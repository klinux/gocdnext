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
//   - compute once on the server and embed that as the initial
//     text (so the SSR HTML is stable);
//   - suppress hydration warning on the text element only, because
//     the client's first-paint compute can legitimately differ by
//     a second;
//   - after mount, tick every second while `finishedAt` is missing
//     so the label stays live without needing a server refresh.
export function LiveDuration({
  startedAt,
  finishedAt,
  fallback = "—",
  className,
}: Props) {
  if (!startedAt) return <span className={className}>{fallback}</span>;

  const server = formatDurationSeconds(durationBetween(startedAt, finishedAt));
  const [label, setLabel] = useState(server);

  useEffect(() => {
    setLabel(formatDurationSeconds(durationBetween(startedAt, finishedAt)));
    if (finishedAt) return; // terminal → no ticking needed
    const id = setInterval(() => {
      setLabel(formatDurationSeconds(durationBetween(startedAt, finishedAt)));
    }, 1000);
    return () => clearInterval(id);
  }, [startedAt, finishedAt]);

  return (
    <span className={className} suppressHydrationWarning>
      {label}
    </span>
  );
}
