import Link from "next/link";
import type { Route } from "next";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { durationBetween, formatDurationSeconds } from "@/lib/format";
import type { GlobalRunSummary, RunSummary } from "@/types/api";

// Any row we might want to render — either a GlobalRunSummary
// (global /runs, dashboard) or a plain RunSummary (project-scope
// runs where the project slug is redundant because it's in the
// URL). The table narrows per-variant below.
type Row = RunSummary | GlobalRunSummary;

type Props = {
  runs: Row[];
  // "global" shows the project slug alongside the pipeline name
  // (used by /runs and the dashboard). "project" omits the slug
  // because the surrounding page already makes the project
  // context obvious.
  variant?: "global" | "project";
  // Empty-state copy — callers customize per page (e.g.
  // "No runs match your filters." vs. "No runs yet for this
  // project.").
  emptyMessage?: string;
};

// RunsTable is the single visual source of truth for "list of
// runs" across /runs, /projects/[slug]/runs, and the dashboard
// Recent activity card. Each variant hides one column but
// otherwise renders identically so the user's scan pattern
// carries between pages. Rows link to /runs/{id} via the
// Project/Pipeline cell (keyboard accessible) — we don't wrap
// the entire row in a Link because nested clickable regions
// (status pill, etc.) clash with row-level navigation.
export function RunsTable({
  runs,
  variant = "global",
  emptyMessage = "No runs to show.",
}: Props) {
  if (runs.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-border py-12 text-center text-sm text-muted-foreground">
        {emptyMessage}
      </div>
    );
  }

  const showProject = variant === "global";

  return (
    <div className="overflow-hidden rounded-lg border border-border bg-card">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-[120px]">Status</TableHead>
            <TableHead>
              {showProject ? "Project / Pipeline" : "Pipeline"}
            </TableHead>
            <TableHead className="w-20">#</TableHead>
            <TableHead className="w-28">Cause</TableHead>
            <TableHead className="w-36">Started</TableHead>
            <TableHead className="w-28">Duration</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {runs.map((r) => {
            const dur = formatDurationSeconds(
              durationBetween(r.started_at, r.finished_at),
            );
            const isGlobal = isGlobalRun(r);
            return (
              <TableRow key={r.id} className="font-mono text-xs">
                <TableCell>
                  <StatusBadge status={r.status} />
                </TableCell>
                <TableCell className="truncate">
                  <Link
                    href={`/runs/${r.id}` as Route}
                    className="hover:underline"
                  >
                    {showProject && isGlobal ? (
                      <>
                        <span className="text-muted-foreground">
                          {r.project_slug}
                        </span>{" "}
                        / {r.pipeline_name}
                      </>
                    ) : (
                      r.pipeline_name
                    )}
                  </Link>
                </TableCell>
                <TableCell className="font-semibold">#{r.counter}</TableCell>
                <TableCell className="text-muted-foreground">
                  {r.cause}
                </TableCell>
                <TableCell>
                  <RelativeTime at={r.started_at ?? r.created_at} />
                </TableCell>
                <TableCell>{dur}</TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}

// isGlobalRun narrows Row to GlobalRunSummary so the project
// column renders safely when the variant asks for it. A plain
// RunSummary in global mode degrades to pipeline-only (same as
// project variant), but the common case is that /runs always
// hands us globals.
function isGlobalRun(r: Row): r is GlobalRunSummary {
  return "project_slug" in r && typeof r.project_slug === "string";
}
