import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDurationSeconds } from "@/lib/format";
import type { BuildTimesReport } from "@/server/queries/analytics";

// fmtBuild formats a build-duration percentile at second granularity (a build of
// 5m30s shouldn't round to "6m" like the coarse DORA formatter); an empty/zero
// percentile renders as "—".
export function fmtBuild(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "—";
  return formatDurationSeconds(seconds);
}

function Stat({ pct, seconds }: { pct: string; seconds: number }) {
  return (
    <div className="flex-1 rounded-lg border border-border bg-card px-4 py-3">
      <div className="font-mono text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
        {pct}
      </div>
      <div className="mt-1 text-2xl font-bold tabular-nums">{fmtBuild(seconds)}</div>
    </div>
  );
}

// DoraBuildTimes is the "build time" panel: p50/p85/p95 of build-pipeline
// duration over the window, org-wide plus a per-group breakdown. A build is a
// successful, non-PR, non-deploy run that produced an artifact — so it measures
// the pipelines that build the artifact/image, free of PR-validation noise and
// deploy time.
export function DoraBuildTimes({
  report,
  groupKey,
}: {
  report: BuildTimesReport;
  groupKey: string;
}) {
  if (report.overall.builds === 0) {
    return (
      <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
        No build runs in this window. Build times come from{" "}
        <span className="font-medium">successful, non-PR runs that produced an artifact</span>{" "}
        (not deploys) — so a project needs a build pipeline that uploads an{" "}
        <span className="font-mono">artifacts:</span> output.
      </p>
    );
  }
  return (
    <div className="space-y-3">
      <div className="flex flex-col gap-2.5 sm:flex-row">
        <Stat pct="p50 (median)" seconds={report.overall.p50_seconds} />
        <Stat pct="p85" seconds={report.overall.p85_seconds} />
        <Stat pct="p95" seconds={report.overall.p95_seconds} />
      </div>
      <p className="text-xs text-muted-foreground">
        {report.overall.builds} build{report.overall.builds === 1 ? "" : "s"} over{" "}
        {report.window_days} days · successful non-PR runs that produced an artifact,
        excluding deploys.
      </p>

      {report.groups.length > 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="border-b border-border px-3.5 py-2.5">
            <h3 className="text-sm font-semibold">Build time by {groupKey}</h3>
          </div>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Group</TableHead>
                <TableHead className="text-right">Builds</TableHead>
                <TableHead className="text-right">p50</TableHead>
                <TableHead className="text-right">p85</TableHead>
                <TableHead className="text-right">p95</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {report.groups.map((g) => (
                <TableRow key={g.group}>
                  <TableCell className="font-medium">{g.group}</TableCell>
                  <TableCell className="text-right tabular-nums">{g.builds}</TableCell>
                  <TableCell className="text-right tabular-nums">{fmtBuild(g.p50_seconds)}</TableCell>
                  <TableCell className="text-right tabular-nums text-muted-foreground">
                    {fmtBuild(g.p85_seconds)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums text-muted-foreground">
                    {fmtBuild(g.p95_seconds)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ) : null}
    </div>
  );
}
