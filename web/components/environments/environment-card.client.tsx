"use client";

import { useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import {
  ChevronDown,
  ChevronUp,
  Eye,
  GitBranch,
  Pencil,
  Plus,
  RefreshCw,
  Rocket,
  RotateCcw,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { DeployTargetDialog } from "@/components/environments/deploy-target-dialog.client";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { RollbackButton } from "@/components/environments/rollback-button.client";
import { useDeployWatch } from "@/components/environments/deploy-watches-provider.client";
import { NativeWatchChip } from "@/components/environments/native-watch-chip.client";
import { RolloutGatePrompt } from "@/components/environments/rollout-gate-buttons.client";
import { statusTone, type StatusTone } from "@/lib/status";
import { cn } from "@/lib/utils";
import type {
  DeploymentRecord,
  DeploymentsList,
  DeployTarget,
  EnvironmentSummary,
} from "@/types/api";

type Props = {
  slug: string;
  environment: EnvironmentSummary;
  // The registered native deploy target (ADR-0001), when this env has one AND the
  // viewer may see it — the query is maintainer-gated, so viewers get undefined and
  // the native row is omitted.
  deployTarget?: DeployTarget;
  // Whether the current user may register/edit/remove native targets (maintainer
  // or admin, or auth disabled). Gates the edit/add affordances — absence of a
  // target alone is ambiguous (viewer vs maintainer-with-no-target).
  canManage: boolean;
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

export function EnvironmentCard({
  slug,
  environment,
  deployTarget,
  canManage,
  apiBaseURL,
}: Props) {
  const { current } = environment;
  const tone: StatusTone = current ? statusTone(current.status) : "neutral";
  const watch = useDeployWatch(environment.name);
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
        <div className="flex items-center gap-2">
          {watch ? <NativeWatchChip watch={watch} /> : null}
          {current ? (
            <StatusBadge status={current.status} />
          ) : (
            <span className="text-xs text-muted-foreground">no deploys yet</span>
          )}
        </div>
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

        {/* Armed canary gate (ADR-0001 Phase 2): the approval prompt + Approve/Reject.
            The server enforces the approvers allow-list + the gate_id token. */}
        {watch?.gate_id && !watch.gate_decision ? (
          <RolloutGatePrompt slug={slug} watch={watch} environment={environment.name} />
        ) : null}

        {deployTarget ? (
          <NativeTargetRow
            slug={slug}
            target={deployTarget}
            canManage={canManage}
          />
        ) : canManage ? (
          <DeployTargetDialog
            slug={slug}
            presetEnvironment={environment.name}
            trigger={
              <Button
                variant="outline"
                size="sm"
                className="h-7 w-full justify-start text-xs text-muted-foreground"
              >
                <Plus className="mr-1 size-3.5" aria-hidden /> Add native target
              </Button>
            }
          />
        ) : null}

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

const PROVIDER_LABELS: Record<string, string> = { argocd: "ArgoCD" };

// The registered native provider target for this env. Maintainer-only (the parent
// only passes it when the maintainer-gated fetch succeeded). Config, not live state —
// live sync/degraded status lands in a later increment via a polled endpoint. When
// the viewer may manage targets, an inline Edit opens the dialog (edit + remove).
function NativeTargetRow({
  slug,
  target,
  canManage,
}: {
  slug: string;
  target: DeployTarget;
  canManage: boolean;
}) {
  const SyncIcon = target.sync_mode === "observe" ? Eye : RefreshCw;
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md border border-border/60 bg-muted/30 px-2.5 py-1.5 text-xs text-muted-foreground">
      <span className="font-medium text-foreground">Native</span>
      <Dot />
      <span>{PROVIDER_LABELS[target.provider] ?? target.provider}</span>
      <Dot />
      <span>
        app <span className="font-mono text-foreground">{target.application}</span>
      </span>
      <Dot />
      <span>
        cluster{" "}
        <span className="font-mono text-foreground">{target.cluster}</span>
      </span>
      <Dot />
      <span className="inline-flex items-center gap-1">
        <SyncIcon className="size-3" aria-hidden />
        {target.sync_mode}
      </span>
      {target.rollout_aware ? (
        <>
          <Dot />
          <span className="inline-flex items-center gap-1 text-teal-600 dark:text-teal-400">
            <GitBranch className="size-3" aria-hidden />
            rollouts
          </span>
        </>
      ) : null}
      {canManage ? (
        <DeployTargetDialog
          slug={slug}
          initial={target}
          trigger={
            <Button
              variant="ghost"
              size="icon"
              className="ml-auto size-6 text-muted-foreground"
              aria-label={`Edit native target for ${target.environment}`}
            >
              <Pencil className="size-3.5" aria-hidden />
            </Button>
          }
        />
      ) : null}
    </div>
  );
}

function Dot() {
  return (
    <span aria-hidden className="text-muted-foreground/50">
      ·
    </span>
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
