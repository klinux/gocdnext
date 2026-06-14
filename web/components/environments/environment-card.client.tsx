"use client";

import { useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import { ChevronDown, ChevronUp, Rocket, RotateCcw } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { RollbackButton } from "@/components/environments/rollback-button.client";
import { statusTone, type StatusTone } from "@/lib/status";
import { cn } from "@/lib/utils";
import type { DeploymentRecord, DeploymentsList, EnvironmentSummary } from "@/types/api";

type Props = {
  slug: string;
  environment: EnvironmentSummary;
  // Browser-facing API base; "" = same-origin. Threaded from the RSC
  // page so the lazy history fetch hits the right host.
  apiBaseURL: string;
};

// Left-border accent by the current deploy's tone — mirrors
// pipeline-card so a wall of environments scans at a glance.
const borderToneClasses: Record<StatusTone, string> = {
  success: "border-l-emerald-500/70",
  failed: "border-l-red-500",
  running: "border-l-sky-500",
  queued: "border-l-amber-500",
  warning: "border-l-amber-500",
  awaiting: "border-l-amber-500",
  canceled: "border-l-muted-foreground/60",
  skipped: "border-l-border",
  neutral: "border-l-border",
};

type HistoryState =
  | { phase: "idle" }
  | { phase: "loading" }
  | { phase: "loaded"; rows: DeploymentRecord[] }
  | { phase: "error"; message: string };

export function EnvironmentCard({ slug, environment, apiBaseURL }: Props) {
  const { current } = environment;
  const tone: StatusTone = current ? statusTone(current.status) : "neutral";
  const [history, setHistory] = useState<HistoryState>({ phase: "idle" });
  const [open, setOpen] = useState(false);

  async function toggleHistory() {
    if (open) {
      setOpen(false);
      return;
    }
    setOpen(true);
    // Fetch once, then keep the result while toggling open/closed.
    if (history.phase === "idle" || history.phase === "error") {
      setHistory({ phase: "loading" });
      try {
        // Trim a trailing slash so a configured base ending in "/"
        // doesn't produce "host//api/v1/..." — matches the other lazy
        // fetchers' normalization.
        const base = apiBaseURL.replace(/\/+$/, "");
        const res = await fetch(
          `${base}/api/v1/projects/${encodeURIComponent(slug)}/environments/${encodeURIComponent(environment.id)}/deployments`,
          { credentials: "include", headers: { Accept: "application/json" } },
        );
        if (!res.ok) {
          throw new Error(`server returned ${res.status}`);
        }
        const data = (await res.json()) as DeploymentsList;
        setHistory({ phase: "loaded", rows: data.deployments });
      } catch (err) {
        setHistory({
          phase: "error",
          message: err instanceof Error ? err.message : "failed to load history",
        });
      }
    }
  }

  return (
    <Card className={cn("border-l-4", borderToneClasses[tone])}>
      <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
        <CardTitle className="flex items-center gap-2 text-base">
          <Rocket className="size-4 text-muted-foreground" aria-hidden />
          {environment.name}
        </CardTitle>
        {current ? (
          <StatusBadge status={current.status} />
        ) : (
          <span className="text-xs text-muted-foreground">no deploys yet</span>
        )}
      </CardHeader>

      <CardContent className="space-y-3">
        {environment.description ? (
          <p className="text-sm text-muted-foreground">{environment.description}</p>
        ) : null}

        {current ? (
          <CurrentDeploy slug={slug} current={current} />
        ) : (
          <p className="text-sm text-muted-foreground">
            Nothing has shipped to this environment yet.
          </p>
        )}

        <div>
          <Button
            variant="ghost"
            size="sm"
            className="-ml-2 h-7 text-xs text-muted-foreground"
            onClick={toggleHistory}
            aria-expanded={open}
          >
            {open ? (
              <ChevronUp className="size-3.5" aria-hidden />
            ) : (
              <ChevronDown className="size-3.5" aria-hidden />
            )}
            History
          </Button>
          {open ? (
            <DeployHistory
              state={history}
              slug={slug}
              environmentId={environment.id}
              environmentName={environment.name}
            />
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}

function CurrentDeploy({ slug, current }: { slug: string; current: DeploymentRecord }) {
  return (
    <div className="space-y-1">
      <div className="flex items-center gap-2">
        {/* break-all + min-w-0: long OCI tags/digests wrap instead of
            pushing the rollback badge off the card. */}
        <span className="min-w-0 break-all font-mono text-sm font-medium">
          {current.version}
        </span>
        {current.is_rollback ? <RollbackBadge /> : null}
      </div>
      <p className="text-xs text-muted-foreground">
        deployed <RelativeTime at={current.finished_at} fallback="—" />
        {current.deployed_by ? ` by ${current.deployed_by}` : null}
        {current.run_id ? (
          <>
            {" · "}
            <RunLink runId={current.run_id} />
          </>
        ) : null}
      </p>
    </div>
  );
}

function DeployHistory({
  state,
  slug,
  environmentId,
  environmentName,
}: {
  state: HistoryState;
  slug: string;
  environmentId: string;
  environmentName: string;
}) {
  if (state.phase === "loading") {
    return <p className="mt-2 text-xs text-muted-foreground">Loading…</p>;
  }
  if (state.phase === "error") {
    return (
      <p className="mt-2 text-xs text-red-500">
        Couldn&apos;t load history ({state.message}).
      </p>
    );
  }
  if (state.phase !== "loaded" || state.rows.length === 0) {
    return <p className="mt-2 text-xs text-muted-foreground">No deploys recorded.</p>;
  }
  return (
    <ol className="mt-2 space-y-2 border-l border-border pl-3">
      {state.rows.map((d) => (
        <li key={d.id} className="flex items-start justify-between gap-2 text-xs">
          <span className="flex min-w-0 items-center gap-2">
            <span className="min-w-0 break-all font-mono">{d.version}</span>
            {d.is_rollback ? <RollbackBadge /> : null}
          </span>
          <span className="flex shrink-0 items-center gap-2 text-muted-foreground">
            <StatusBadge status={d.status} className="text-[10px]" />
            <RelativeTime at={d.finished_at ?? d.created_at} fallback="—" />
            {d.run_id ? <RunLink runId={d.run_id} /> : null}
            {/* Roll back is offered only for a successful deploy whose
                run still exists (a GC'd run has no job to re-run). */}
            {d.status === "success" && d.run_id ? (
              <RollbackButton
                slug={slug}
                environmentId={environmentId}
                environmentName={environmentName}
                revisionId={d.id}
                version={d.version}
              />
            ) : null}
          </span>
        </li>
      ))}
    </ol>
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
