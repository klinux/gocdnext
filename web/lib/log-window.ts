import type { LogLine } from "@/types/api";

// The subset of a job's fields the log-window math needs. Kept structural so
// both JobCard (full run) and JobDetailSheet (drawer) can pass their JobDetail.
export type LogWindowFields = {
  logs?: LogLine[];
  logs_head?: LogLine[];
  logs_omitted?: number;
  logs_total?: number;
};

export type LogWindowCounts = {
  // shown: head + tail lines actually rendered.
  shown: number;
  // total: the honest "Y" in "Logs (X of Y)".
  total: number;
  // omitted: the hidden middle — the "(N omitted)" divider count.
  omitted: number;
};

// logWindowCounts derives the honest counts for a job's log window, tolerant of
// EVERY fetch shape so the log pane can't lie:
//
//   - Archived / tail-only / cursor responses carry `logs_total` but no
//     `logs_omitted` (the server only computes omitted when a head is fetched).
//     Deriving omitted = total - shown here is what surfaces the "(N omitted)"
//     divider in the drawer and full run alike.
//   - The SSE stream appends lines to the tail between 2s polls while
//     `logs_total` still holds the LAST poll's value; clamping total to at
//     least the visible count avoids an impossible "101 of 100".
//   - An older server that only sends `logs_omitted` (no `logs_total`) falls
//     back to shown + logs_omitted.
//
// Single source of truth so JobCard and JobDetailSheet stay in lockstep.
export function logWindowCounts(job: LogWindowFields): LogWindowCounts {
  const shown = (job.logs_head?.length ?? 0) + (job.logs?.length ?? 0);
  const total = Math.max(
    job.logs_total ?? 0,
    shown + (job.logs_omitted ?? 0),
    shown,
  );
  return { shown, total, omitted: Math.max(0, total - shown) };
}
