// Display helpers: durations between started/finished, relative timestamps
// for the dashboard's "7 minutes ago" labels. Kept framework-agnostic so the
// same helpers can live in a Node test without importing next/react.

export function formatDurationSeconds(
  seconds: number | null | undefined,
): string {
  if (seconds == null) return "—";
  if (seconds === 0) return "0s";
  if (seconds < 1) return "< 1s";
  const total = Math.floor(seconds);
  if (total < 60) return `${total}s`;
  if (total < 3600) {
    const m = Math.floor(total / 60);
    const s = total % 60;
    return `${m}m ${s}s`;
  }
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  return `${h}h ${m}m`;
}

export function formatRelative(
  input: Date | string | null | undefined,
  now: Date = new Date(),
): string {
  if (!input) return "—";
  const d = typeof input === "string" ? new Date(input) : input;
  const diffMs = now.getTime() - d.getTime();
  if (diffMs < 0) return "in the future";
  const secs = Math.floor(diffMs / 1000);
  if (secs < 10) return "just now";
  if (secs < 60) return `${secs} seconds ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins} ${plural(mins, "minute")} ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours} ${plural(hours, "hour")} ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days} ${plural(days, "day")} ago`;
  const months = Math.floor(days / 30);
  if (months < 12) return `${months} ${plural(months, "month")} ago`;
  const years = Math.floor(months / 12);
  return `${years} ${plural(years, "year")} ago`;
}

function plural(n: number, singular: string): string {
  return n === 1 ? singular : `${singular}s`;
}

export function durationBetween(
  startedAt: string | null | undefined,
  finishedAt: string | null | undefined,
): number | null {
  if (!startedAt) return null;
  const start = new Date(startedAt).getTime();
  const end = finishedAt ? new Date(finishedAt).getTime() : Date.now();
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return null;
  return (end - start) / 1000;
}
