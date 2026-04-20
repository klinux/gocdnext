import Link from "next/link";
import type { Metadata, Route } from "next";
import { Activity, ChevronLeft, ChevronRight } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { formatDurationSeconds, durationBetween } from "@/lib/format";
import { listGlobalRuns } from "@/server/queries/projects";

export const metadata: Metadata = {
  title: "Runs — gocdnext",
};

export const dynamic = "force-dynamic";

const PAGE_SIZE = 50;

type SearchParams = {
  status?: string;
  cause?: string;
  project?: string;
  offset?: string;
};

// Valid values for the filter chips. Keep in sync with domain.CauseWebhook
// etc. and domain.RunStatus on the Go side.
const STATUSES = ["queued", "running", "success", "failed", "canceled"] as const;
const CAUSES = ["webhook", "pull_request", "upstream", "manual"] as const;

export default async function RunsListPage({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const sp = await searchParams;
  const offset = sp.offset ? Math.max(0, Number.parseInt(sp.offset, 10)) : 0;
  const status = typeof sp.status === "string" ? sp.status : undefined;
  const cause = typeof sp.cause === "string" ? sp.cause : undefined;
  const project = typeof sp.project === "string" ? sp.project : undefined;

  const data = await listGlobalRuns({
    limit: PAGE_SIZE,
    offset,
    status,
    cause,
    project,
  });

  return (
    <section className="space-y-6">
      <header>
        <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <Activity className="h-6 w-6" aria-hidden />
          Runs
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          {data.total.toLocaleString()} run{data.total === 1 ? "" : "s"} across every project.
        </p>
      </header>

      <FilterBar current={{ status, cause, project }} total={data.total} />

      <Card>
        <CardContent className="p-0">
          {data.runs.length === 0 ? (
            <div className="py-16 text-center text-sm text-muted-foreground">
              No runs match your filters.
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[120px]">Status</TableHead>
                  <TableHead>Project / Pipeline</TableHead>
                  <TableHead className="w-20">#</TableHead>
                  <TableHead className="w-28">Cause</TableHead>
                  <TableHead className="w-36">Started</TableHead>
                  <TableHead className="w-28">Duration</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.runs.map((r) => {
                  const dur = formatDurationSeconds(
                    durationBetween(r.started_at, r.finished_at),
                  );
                  return (
                    <TableRow
                      key={r.id}
                      className="font-mono text-xs cursor-pointer"
                    >
                      <TableCell>
                        <StatusBadge status={r.status} />
                      </TableCell>
                      <TableCell className="truncate">
                        <Link
                          href={`/runs/${r.id}` as Route}
                          className="hover:underline"
                        >
                          <span className="text-muted-foreground">
                            {r.project_slug}
                          </span>{" "}
                          / {r.pipeline_name}
                        </Link>
                      </TableCell>
                      <TableCell className="font-semibold">
                        #{r.counter}
                      </TableCell>
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
          )}
        </CardContent>
      </Card>

      <Pagination offset={offset} total={data.total} params={sp} />
    </section>
  );
}

function FilterBar({
  current,
  total,
}: {
  current: { status?: string; cause?: string; project?: string };
  total: number;
}) {
  const { status, cause, project } = current;
  const anyActive = Boolean(status || cause || project);
  return (
    <div className="flex flex-wrap items-center gap-2">
      <FilterGroup label="Status" param="status" value={status} options={STATUSES} />
      <FilterGroup label="Cause" param="cause" value={cause} options={CAUSES} />
      {project ? (
        <Link
          href={qs({ status, cause })}
          className="inline-flex items-center gap-1 rounded-md border border-border bg-muted/40 px-2 py-1 text-xs"
        >
          project: <span className="font-mono">{project}</span>
          <span className="text-muted-foreground ml-1">✕</span>
        </Link>
      ) : null}
      <span className="ml-auto text-xs text-muted-foreground tabular-nums">
        {total.toLocaleString()} total
      </span>
      {anyActive ? (
        <Button
          variant="ghost"
          size="sm"
          className="h-7 text-xs"
          render={<Link href={"/runs" as Route}>Clear filters</Link>}
        />
      ) : null}
    </div>
  );
}

function FilterGroup<T extends string>({
  label,
  param,
  value,
  options,
}: {
  label: string;
  param: "status" | "cause";
  value: string | undefined;
  options: readonly T[];
}) {
  return (
    <div className="flex flex-wrap items-center gap-1">
      <span className="text-[11px] uppercase tracking-wide text-muted-foreground mr-1">
        {label}
      </span>
      {options.map((opt) => {
        const active = value === opt;
        const nextQS = qs(active ? { [param]: undefined } : { [param]: opt });
        return (
          <Link
            key={opt}
            href={nextQS}
            className="no-underline"
            aria-current={active ? "true" : undefined}
          >
            <Badge
              variant={active ? "default" : "outline"}
              className="cursor-pointer"
            >
              {opt}
            </Badge>
          </Link>
        );
      })}
    </div>
  );
}

function Pagination({
  offset,
  total,
  params,
}: {
  offset: number;
  total: number;
  params: SearchParams;
}) {
  const hasPrev = offset > 0;
  const hasNext = offset + PAGE_SIZE < total;
  if (!hasPrev && !hasNext) return null;

  const prev = Math.max(0, offset - PAGE_SIZE);
  const next = offset + PAGE_SIZE;
  return (
    <div className="flex items-center justify-between">
      <p className="text-xs text-muted-foreground tabular-nums">
        Showing {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} of {total}
      </p>
      <div className="flex gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={!hasPrev}
          render={
            hasPrev ? (
              <Link href={qs({ ...params, offset: String(prev) })}>
                <ChevronLeft className="h-3.5 w-3.5" />
                Prev
              </Link>
            ) : (
              <span>
                <ChevronLeft className="h-3.5 w-3.5" />
                Prev
              </span>
            )
          }
        />
        <Button
          variant="outline"
          size="sm"
          disabled={!hasNext}
          render={
            hasNext ? (
              <Link href={qs({ ...params, offset: String(next) })}>
                Next
                <ChevronRight className="h-3.5 w-3.5" />
              </Link>
            ) : (
              <span>
                Next
                <ChevronRight className="h-3.5 w-3.5" />
              </span>
            )
          }
        />
      </div>
    </div>
  );
}

// qs builds a `/runs?...` Route preserving all non-undefined keys.
// Kept local so the filter components don't have to import a URL
// helper for one use case.
function qs(params: Record<string, string | undefined>): Route {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v != null && v !== "") q.set(k, v);
  }
  const s = q.toString();
  return (s ? `/runs?${s}` : "/runs") as Route;
}
