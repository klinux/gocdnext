import Link from "next/link";
import type { Metadata, Route } from "next";
import { Activity, X } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Pagination } from "@/components/shared/pagination";
import { RunsTable } from "@/components/runs/runs-table";
import { listGlobalRuns } from "@/server/queries/projects";
import { cn } from "@/lib/utils";

export const metadata: Metadata = {
  title: "Runs — gocdnext",
};

export const dynamic = "force-dynamic";

const PAGE_SIZE = 25;

type SearchParams = {
  status?: string;
  cause?: string;
  project?: string;
  offset?: string;
};

// Valid values for the filter chips. Keep in sync with the
// canonical CauseWebhook etc. and RunStatus on the Go side.
// Schedule + poll were added when the project-cron + polling
// features shipped — old chip list missed them.
const STATUSES = [
  "queued",
  "running",
  "success",
  "failed",
  "canceled",
] as const;
const CAUSES = [
  "webhook",
  "pull_request",
  "upstream",
  "manual",
  "schedule",
  "poll",
] as const;

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

  const anyActive = Boolean(status || cause || project);

  return (
    <section className="space-y-5">
      <header>
        <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <Activity className="h-6 w-6" aria-hidden />
          Runs
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          {data.total.toLocaleString()} run{data.total === 1 ? "" : "s"} across
          every project
          {data.runs.length > 0 ? (
            <>
              {" · "}showing {Math.min(offset + 1, data.total)}–
              {Math.min(offset + data.runs.length, data.total)}
            </>
          ) : null}
          .
        </p>
      </header>

      <div className="space-y-2.5 rounded-lg border bg-card p-3">
        <FilterRow
          label="Status"
          param="status"
          value={status}
          options={STATUSES}
          context={{ status, cause, project }}
        />
        <FilterRow
          label="Cause"
          param="cause"
          value={cause}
          options={CAUSES}
          context={{ status, cause, project }}
        />
        {project || anyActive ? (
          <div className="flex flex-wrap items-center gap-2 border-t pt-2.5">
            {project ? (
              <Link
                href={qs({ status, cause })}
                className="inline-flex items-center gap-1 rounded-md border border-border bg-muted/40 px-2 py-1 text-xs hover:bg-muted"
              >
                project: <span className="font-mono">{project}</span>
                <X className="size-3 text-muted-foreground" aria-hidden />
              </Link>
            ) : null}
            {anyActive ? (
              <Button
                variant="ghost"
                size="sm"
                className="ml-auto h-7 text-xs"
                nativeButton={false}
                render={<Link href={"/runs" as Route}>Clear all filters</Link>}
              />
            ) : null}
          </div>
        ) : null}
      </div>

      <RunsTable
        runs={data.runs}
        variant="global"
        emptyMessage={
          anyActive
            ? "No runs match your filters."
            : "No runs yet — push a commit to a connected repo to see one here."
        }
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

function FilterRow<T extends string>({
  label,
  param,
  value,
  options,
  context,
}: {
  label: string;
  param: "status" | "cause";
  value: string | undefined;
  options: readonly T[];
  context: { status?: string; cause?: string; project?: string };
}) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <span className="mr-1 w-14 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      {options.map((opt) => {
        const active = value === opt;
        const next = qs({
          ...context,
          [param]: active ? undefined : opt,
        });
        return (
          <Link
            key={opt}
            href={next}
            className="no-underline"
            aria-current={active ? "true" : undefined}
          >
            <Badge
              variant={active ? "default" : "outline"}
              className={cn(
                "cursor-pointer capitalize transition-colors",
                !active && "hover:bg-muted",
              )}
            >
              {opt}
            </Badge>
          </Link>
        );
      })}
    </div>
  );
}

// qs builds a `/runs?...` Route preserving non-undefined keys.
function qs(params: Record<string, string | undefined>): Route {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v != null && v !== "") q.set(k, v);
  }
  const s = q.toString();
  return (s ? `/runs?${s}` : "/runs") as Route;
}
