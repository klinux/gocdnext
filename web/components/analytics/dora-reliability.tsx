import Link from "next/link";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { cfrTier, fmtDuration, fmtPct } from "@/lib/dora";
import { cn } from "@/lib/utils";
import type {
  ReliabilityHotspot,
  ReliabilityReport,
  ThroughputGroup,
} from "@/server/queries/analytics";

// Success rate is the inverse of change failure: 95% success ≈ 5% failure →
// elite. Reuse the CFR banding so the colour language matches the DORA cards.
function successTone(rate: number): string {
  const t = cfrTier(1 - rate);
  if (t === "elite" || t === "high") return "text-status-success";
  if (t === "medium") return "text-status-warning";
  return "text-status-failed";
}

// fmtShort shows sub-minute waits in seconds (a 30s queue isn't "1m"); falls
// back to the shared duration formatter above a minute. Zero → "—".
function fmtShort(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "—";
  if (seconds < 60) return `${Math.round(seconds)}s`;
  return fmtDuration(seconds);
}

function fmtPerDay(n: number): string {
  if (n >= 10) return Math.round(n).toString();
  return n.toFixed(1).replace(/\.0$/, "");
}

function ThroughputTable({
  groups,
  groupKey,
}: {
  groups: ThroughputGroup[];
  groupKey: string;
}) {
  return (
    <div className="rounded-lg border border-border bg-card">
      <div className="border-b border-border px-3.5 py-2.5">
        <h3 className="text-sm font-semibold">Throughput by {groupKey}</h3>
        <p className="text-xs text-muted-foreground">
          Run volume &amp; reliability across all pipelines in each group.
        </p>
      </div>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Group</TableHead>
            <TableHead className="text-right">Runs</TableHead>
            <TableHead className="text-right">Per day</TableHead>
            <TableHead className="text-right">Success</TableHead>
            <TableHead className="text-right">Queue p50</TableHead>
            <TableHead className="text-right">Duration p50</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {groups.map((g) => (
            <TableRow key={g.group}>
              <TableCell className="font-medium">{g.group}</TableCell>
              <TableCell className="text-right tabular-nums">{g.runs_total}</TableCell>
              <TableCell className="text-right tabular-nums">{fmtPerDay(g.runs_per_day)}</TableCell>
              <TableCell
                className={cn("text-right font-medium tabular-nums", successTone(g.success_rate))}
              >
                {fmtPct(g.success_rate)}
              </TableCell>
              <TableCell className="text-right tabular-nums text-muted-foreground">
                {fmtShort(g.queue_wait_p50_seconds)}
              </TableCell>
              <TableCell className="text-right tabular-nums text-muted-foreground">
                {fmtShort(g.duration_p50_seconds)}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function HotspotRow({ h }: { h: ReliabilityHotspot }) {
  return (
    <li className="flex items-center gap-3 px-3.5 py-2.5">
      <div className="min-w-0 flex-1">
        <Link
          href={`/projects/${h.project_slug}`}
          className="block truncate text-sm font-medium hover:underline"
        >
          {h.project}
          <span className="text-muted-foreground"> / {h.pipeline}</span>
        </Link>
        <div className="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-muted">
          <div
            className="h-full rounded-full bg-status-failed"
            style={{ width: `${Math.round(h.failure_rate * 100)}%` }}
          />
        </div>
      </div>
      <div className="shrink-0 text-right">
        <div className="text-sm font-semibold tabular-nums text-status-failed">
          {fmtPct(h.failure_rate)}
        </div>
        <div className="text-xs tabular-nums text-muted-foreground">
          {h.runs_failed}/{h.runs_total} failed
        </div>
      </div>
    </li>
  );
}

function Hotspots({ hotspots }: { hotspots: ReliabilityHotspot[] }) {
  return (
    <div className="rounded-lg border border-border bg-card">
      <div className="border-b border-border px-3.5 py-2.5">
        <h3 className="text-sm font-semibold">Reliability hotspots</h3>
        <p className="text-xs text-muted-foreground">
          Pipelines that fail most (≥5 runs in window), worst first.
        </p>
      </div>
      {hotspots.length === 0 ? (
        <p className="px-3.5 py-6 text-center text-sm text-muted-foreground">
          No failing pipelines in this window. Clean slate. ✨
        </p>
      ) : (
        <ul className="divide-y divide-border">
          {hotspots.map((h) => (
            <HotspotRow key={`${h.project_slug}/${h.pipeline}`} h={h} />
          ))}
        </ul>
      )}
    </div>
  );
}

// DoraReliability is the "throughput & reliability" section: per-group run
// volume + success rate, plus the worst-failing pipelines. Run-based, so it
// ignores the deploy-environment filter — when that filter is active, say so
// (envFiltered) to avoid reading this section as environment-scoped too.
export function DoraReliability({
  report,
  groupKey,
  envFiltered = false,
}: {
  report: ReliabilityReport;
  groupKey: string;
  envFiltered?: boolean;
}) {
  if (report.groups.length === 0) {
    return (
      <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
        No finished runs in this window for any{" "}
        <span className="font-mono">{groupKey}</span> group.
      </p>
    );
  }
  return (
    <div className="space-y-2">
      {envFiltered ? (
        <p className="text-xs text-muted-foreground">
          Run-based — not scoped by the environment filter (all environments).
        </p>
      ) : null}
      <div className="grid gap-3.5 lg:grid-cols-2">
        <ThroughputTable groups={report.groups} groupKey={groupKey} />
        <Hotspots hotspots={report.hotspots} />
      </div>
    </div>
  );
}
