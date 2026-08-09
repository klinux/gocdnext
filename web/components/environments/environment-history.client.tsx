"use client";

import { useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import { Loader2, RotateCcw } from "lucide-react";

import { RollbackButton } from "@/components/environments/rollback-button.client";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { Button } from "@/components/ui/button";
import type { DeploymentRecord, DeploymentsList } from "@/types/api";

type Props = {
  slug: string;
  environmentId: string;
  environmentName: string;
  currentRevisionId?: string;
  initial: DeploymentsList;
  apiBaseURL: string;
  canManage: boolean;
};

export function EnvironmentHistory({
  slug,
  environmentId,
  environmentName,
  currentRevisionId,
  initial,
  apiBaseURL,
  canManage,
}: Props) {
  const [rows, setRows] = useState<DeploymentRecord[]>(initial.deployments);
  const [nextCursor, setNextCursor] = useState(initial.next_cursor);
  const [total, setTotal] = useState(
    initial.total ?? initial.environment?.total_deploys ?? initial.deployments.length,
  );
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function loadMore() {
    if (!nextCursor || loading) return;
    setLoading(true);
    setError(null);
    try {
      const base = apiBaseURL.replace(/\/+$/, "");
      const qs = new URLSearchParams({ limit: "50", cursor: nextCursor });
      const res = await fetch(
        `${base}/api/v1/projects/${encodeURIComponent(slug)}/environments/${encodeURIComponent(environmentId)}/deployments?${qs.toString()}`,
        { credentials: "include", headers: { Accept: "application/json" } },
      );
      if (!res.ok) throw new Error(`server returned ${res.status}`);
      const data = (await res.json()) as DeploymentsList;
      setRows((prev) => [...prev, ...data.deployments]);
      setNextCursor(data.next_cursor);
      setTotal(data.total ?? data.environment?.total_deploys ?? total);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load history");
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="overflow-hidden rounded-lg border border-border bg-card">
      {rows.length === 0 ? (
        <p className="px-4 py-10 text-center text-sm text-muted-foreground">
          No deploys recorded.
        </p>
      ) : (
        <ol className="divide-y divide-border/60">
          {rows.map((deployment) => (
            <HistoryRow
              key={deployment.id}
              slug={slug}
              environmentId={environmentId}
              environmentName={environmentName}
              deployment={deployment}
              current={deployment.id === currentRevisionId}
              canManage={canManage}
            />
          ))}
        </ol>
      )}

      <div className="flex flex-wrap items-center justify-between gap-3 border-t border-border/60 bg-muted/40 px-4 py-3 text-sm text-muted-foreground">
        <span>
          Showing {rows.length} of {total} deployment{total === 1 ? "" : "s"}
        </span>
        <div className="flex items-center gap-2">
          {error ? <span className="text-xs text-red-500">{error}</span> : null}
          {nextCursor ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => void loadMore()}
              disabled={loading}
            >
              {loading ? (
                <Loader2 className="size-3.5 animate-spin" aria-hidden />
              ) : null}
              Load more
            </Button>
          ) : null}
        </div>
      </div>
    </div>
  );
}

function HistoryRow({
  slug,
  environmentId,
  environmentName,
  deployment,
  current,
  canManage,
}: {
  slug: string;
  environmentId: string;
  environmentName: string;
  deployment: DeploymentRecord;
  current: boolean;
  canManage: boolean;
}) {
  return (
    <li className="grid gap-3 px-4 py-3 text-sm md:grid-cols-[minmax(0,1fr)_auto] md:items-center">
      <div className="min-w-0 space-y-1">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="min-w-0 break-all font-mono font-semibold text-foreground">
            {deployment.version}
          </span>
          {current ? (
            <span className="rounded-4xl bg-primary/10 px-1.5 py-0.5 text-[10px] font-medium text-primary">
              current
            </span>
          ) : null}
          {deployment.is_rollback ? <RollbackBadge /> : null}
        </div>
        <p className="text-xs text-muted-foreground">
          {deployment.deployed_by ? `by ${deployment.deployed_by}` : "no actor recorded"}
        </p>
      </div>
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground md:justify-end">
        <StatusBadge status={deployment.status} className="text-[10px]" />
        <RelativeTime
          at={deployment.finished_at ?? deployment.created_at}
          fallback="-"
        />
        {deployment.run_id ? <RunLink runId={deployment.run_id} /> : null}
        {canManage && deployment.status === "success" && deployment.run_id ? (
          <RollbackButton
            slug={slug}
            environmentId={environmentId}
            environmentName={environmentName}
            revisionId={deployment.id}
            version={deployment.version}
          />
        ) : null}
      </div>
    </li>
  );
}

function RunLink({ runId }: { runId: string }) {
  return (
    <Link href={`/runs/${runId}` as Route} className="hover:underline">
      run
    </Link>
  );
}

function RollbackBadge() {
  return (
    <span className="inline-flex items-center gap-1 rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] font-medium text-amber-600 dark:text-amber-400">
      <RotateCcw className="size-3" aria-hidden />
      rollback
    </span>
  );
}
