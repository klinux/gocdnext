import Link from "next/link";
import type { Metadata, Route } from "next";
import { Activity } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Pagination } from "@/components/shared/pagination";
import { RunsTable } from "@/components/runs/runs-table";
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

// Valid values for the filter chips. Keep in sync with
// domain.CauseWebhook etc. and domain.RunStatus on the Go side.
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

      <RunsTable
        runs={data.runs}
        variant="global"
        emptyMessage="No runs match your filters."
      />

      <Pagination
        offset={offset}
        total={data.total}
        pageSize={PAGE_SIZE}
        basePath="/runs"
        params={{ status, cause, project }}
      />
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
          nativeButton={false}
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

// qs builds a `/runs?...` Route preserving all non-undefined
// keys. Kept local so the filter components don't have to import
// a URL helper for one use case.
function qs(params: Record<string, string | undefined>): Route {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v != null && v !== "") q.set(k, v);
  }
  const s = q.toString();
  return (s ? `/runs?${s}` : "/runs") as Route;
}
