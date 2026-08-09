"use client";

import {
  forwardRef,
  type ReactNode,
  useCallback,
  useImperativeHandle,
  useState,
  useTransition,
} from "react";
import Link from "next/link";
import type { Route } from "next";
import {
  ChevronDown,
  Eye,
  GitBranch,
  Loader2,
  MoreHorizontal,
  Pencil,
  Plus,
  RefreshCw,
  Rocket,
  RotateCcw,
  Snowflake,
  Trash2,
} from "lucide-react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { DeployTargetDialog } from "@/components/environments/deploy-target-dialog.client";
import { RemoveEnvironment } from "@/components/environments/remove-environment-button.client";
import {
  Card,
  CardContent,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { RollbackButton } from "@/components/environments/rollback-button.client";
import { useDeployWatch } from "@/components/environments/deploy-watches-provider.client";
import { NativeWatchChip } from "@/components/environments/native-watch-chip.client";
import { RolloutGatePrompt } from "@/components/environments/rollout-gate-buttons.client";
import { FreezeDialog } from "@/components/environments/freeze-dialog.client";
import { unfreezeEnvironment } from "@/server/actions/environments";
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
  // Whether the current user is an admin (or auth disabled). Gates the
  // destructive "Remove environment" action — the server cascade drops deploy
  // history + any gated target, so it's admin-only (tighter than canManage).
  isAdmin: boolean;
  // Browser-facing API base; "" = same-origin. Threaded from the RSC
  // page so the lazy history fetch hits the right host.
  apiBaseURL: string;
  // Optional notification for page-level controls such as "expand histories".
  onHistoryOpenChange?: (open: boolean) => void;
};

export type EnvironmentCardHandle = {
  setHistoryOpen: (open: boolean) => void;
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

const HISTORY_SCROLL_CLASS =
  "max-h-[196px] overflow-y-auto overscroll-contain [scrollbar-width:thin]";

type HistoryState =
  | { phase: "idle" }
  | { phase: "loading" }
  | { phase: "loaded"; rows: DeploymentRecord[]; total: number }
  | { phase: "error"; message: string };

export const EnvironmentCard = forwardRef<EnvironmentCardHandle, Props>(
  function EnvironmentCard(
    {
      slug,
      environment,
      deployTarget,
      canManage,
      isAdmin,
      apiBaseURL,
      onHistoryOpenChange,
    },
    ref,
  ) {
  const { current } = environment;
  const accentTone: StatusTone = environment.frozen
    ? "warning"
    : current
      ? statusTone(current.status)
      : "neutral";
  const watch = useDeployWatch(environment.name);
  const [history, setHistory] = useState<HistoryState>({ phase: "idle" });
  const [open, setOpen] = useState(false);

  // A freeze-only card (#202) has no environments row, so there is nothing to
  // fetch history for, roll back to, or delete. `id` drives all three URLs.
  const environmentId = environment.id;
  const hasRow = environment.has_environment_row && environmentId !== undefined;

  const loadHistory = useCallback(async () => {
    if (!hasRow) return;
    setHistory({ phase: "loading" });
    try {
      // Trim a trailing slash so a configured base ending in "/"
      // doesn't produce "host//api/v1/..." — matches the other lazy
      // fetchers' normalization.
      const base = apiBaseURL.replace(/\/+$/, "");
      const res = await fetch(
        `${base}/api/v1/projects/${encodeURIComponent(slug)}/environments/${encodeURIComponent(environmentId)}/deployments`,
        { credentials: "include", headers: { Accept: "application/json" } },
      );
      if (!res.ok) {
        throw new Error(`server returned ${res.status}`);
      }
      const data = (await res.json()) as DeploymentsList;
      setHistory({
        phase: "loaded",
        rows: data.deployments,
        total: data.total ?? environment.total_deploys ?? data.deployments.length,
      });
    } catch (err) {
      setHistory({
        phase: "error",
        message: err instanceof Error ? err.message : "failed to load history",
      });
    }
  }, [apiBaseURL, environment.total_deploys, environmentId, hasRow, slug]);

  const setHistoryOpenState = useCallback(
    (next: boolean) => {
      if (!hasRow) return;
      setOpen(next);
      onHistoryOpenChange?.(next);
      if (next && (history.phase === "idle" || history.phase === "error")) {
        void loadHistory();
      }
    },
    [hasRow, history.phase, loadHistory, onHistoryOpenChange],
  );

  useImperativeHandle(
    ref,
    () => ({
      setHistoryOpen: setHistoryOpenState,
    }),
    [setHistoryOpenState],
  );

  function toggleHistory() {
    setHistoryOpenState(!open);
  }

  return (
    <Card
      className={cn(
        "gap-0 border-l-4 py-0 transition-shadow hover:shadow-sm",
        borderToneClasses[accentTone],
      )}
    >
      <CardHeader className="flex-row items-start justify-between gap-3 space-y-0 px-4 pb-0 pt-4">
        <CardTitle className="flex min-w-0 items-center gap-2 text-base font-semibold">
          <Rocket className="size-4 text-muted-foreground" aria-hidden />
          <span className="truncate">{environment.name}</span>
        </CardTitle>
        <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">
          {watch ? <NativeWatchChip watch={watch} /> : null}
          {environment.frozen ? (
            <span className="inline-flex h-5 items-center gap-1 rounded-4xl bg-amber-500/10 px-2 text-xs font-medium text-amber-700 dark:text-amber-400">
              <Snowflake className="size-3" aria-hidden />
              Frozen
            </span>
          ) : current ? (
            <StatusBadge status={current.status} />
          ) : (
            <span className="inline-flex h-5 items-center rounded-4xl bg-muted px-2 text-xs font-medium text-muted-foreground">
              no deploys yet
            </span>
          )}
          <EnvironmentActionsMenu
            slug={slug}
            environment={environment}
            deployTarget={deployTarget}
            canManage={canManage}
            isAdmin={isAdmin}
            hasRow={hasRow}
            environmentId={environmentId}
          />
        </div>
      </CardHeader>

      <CardContent className="flex flex-1 flex-col gap-3 px-4 py-3">
        {environment.description ? (
          <p className="text-sm text-muted-foreground">{environment.description}</p>
        ) : null}

        {environment.frozen ? <FrozenBanner environment={environment} /> : null}

        {current ? (
          <CurrentDeploy slug={slug} current={current} />
        ) : hasRow ? (
          <p className="text-sm text-muted-foreground">
            Nothing has shipped to this environment yet.
          </p>
        ) : (
          // Freeze-only: there is no environments row behind this card, so
          // saying "nothing has shipped yet" would imply an environment that
          // doesn't exist. Say what it actually is.
          <p className="text-sm text-muted-foreground">
            This environment doesn&apos;t exist yet — the freeze is holding the
            first deploy that would create it.
          </p>
        )}

        {/* Armed canary gate (ADR-0001 Phase 2): the approval prompt + Approve/Reject.
            The server enforces the approvers allow-list + the gate_id token. */}
        {watch?.gate_id && !watch.gate_decision ? (
          <RolloutGatePrompt slug={slug} watch={watch} canManage={canManage} />
        ) : null}

        {deployTarget ? <NativeTargetRow target={deployTarget} /> : null}
      </CardContent>

      <CardFooter className="mt-auto justify-between gap-2 rounded-none border-t border-border/60 bg-muted/40 px-4 py-2">
        {/* History / Remove address the environments row by id, so they are
            hidden entirely on a freeze-only card — there is no row. */}
        {hasRow ? (
          <Button
            variant="ghost"
            size="sm"
            className="-ml-2 h-7 text-xs font-medium text-muted-foreground"
            onClick={toggleHistory}
            aria-expanded={open}
            aria-label={`History, ${environment.total_deploys.toLocaleString()} deployment${environment.total_deploys === 1 ? "" : "s"}`}
          >
            <ChevronDown
              className={cn("size-3.5 transition-transform", open && "rotate-180")}
              aria-hidden
            />
            History
            <span className="ml-1 rounded-4xl bg-muted px-1.5 py-0.5 text-[10px] font-medium tabular-nums text-muted-foreground">
              {environment.total_deploys.toLocaleString()}
            </span>
          </Button>
        ) : (
          <span aria-hidden className="min-h-7 flex-1" />
        )}
        <span className="flex items-center gap-2">
          {canManage ? (
            <FreezeControl slug={slug} environment={environment} />
          ) : null}
        </span>
      </CardFooter>

      {open && hasRow ? (
        <DeployHistory
          state={history}
          slug={slug}
          environmentId={environmentId}
          environmentName={environment.name}
          currentRevisionId={current?.id}
          onRetry={loadHistory}
        />
      ) : null}
    </Card>
  );
  },
);

function EnvironmentActionsMenu({
  slug,
  environment,
  deployTarget,
  canManage,
  isAdmin,
  hasRow,
  environmentId,
}: {
  slug: string;
  environment: EnvironmentSummary;
  deployTarget?: DeployTarget;
  canManage: boolean;
  isAdmin: boolean;
  hasRow: boolean;
  environmentId?: string;
}) {
  const [targetDialog, setTargetDialog] = useState<"add" | "edit" | null>(null);
  const [removeOpen, setRemoveOpen] = useState(false);
  const [redeployOpen, setRedeployOpen] = useState(false);
  const showTargetAction = canManage;
  const current = environment.current;
  const showRedeploy =
    canManage && hasRow && environmentId !== undefined && current?.run_id !== undefined;
  const redeployEnvironmentId = showRedeploy ? environmentId : undefined;
  const removableEnvironmentId = isAdmin && hasRow ? environmentId : undefined;
  const showRemove = removableEnvironmentId !== undefined;

  if (!showTargetAction && !showRedeploy && !showRemove) return null;

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={`Environment actions for ${environment.name}`}
              className="-mr-1 size-7 text-muted-foreground"
            >
              <MoreHorizontal className="size-4" aria-hidden />
            </Button>
          }
        />
        <DropdownMenuContent align="end" className="min-w-56">
          <div className="px-1.5 py-1 text-xs font-medium text-muted-foreground">
            Environment
          </div>
          {showTargetAction ? (
            <DropdownMenuItem
              className="whitespace-nowrap"
              onClick={() => setTargetDialog(deployTarget ? "edit" : "add")}
            >
              {deployTarget ? (
                <Pencil className="size-3.5" aria-hidden />
              ) : (
                <Plus className="size-3.5" aria-hidden />
              )}
              {deployTarget ? "Edit native target" : "Add native target"}
            </DropdownMenuItem>
          ) : null}
          {showRedeploy ? (
            <DropdownMenuItem
              className="whitespace-nowrap"
              onClick={() => setRedeployOpen(true)}
            >
              <RefreshCw className="size-3.5" aria-hidden />
              Redeploy current version
            </DropdownMenuItem>
          ) : null}
          {(showTargetAction || showRedeploy) && showRemove ? (
            <DropdownMenuSeparator />
          ) : null}
          {showRemove ? (
            <DropdownMenuItem
              variant="destructive"
              className="whitespace-nowrap"
              onClick={() => setRemoveOpen(true)}
            >
              <Trash2 className="size-3.5" aria-hidden />
              Remove environment
            </DropdownMenuItem>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>

      {targetDialog === "edit" && deployTarget ? (
        <DeployTargetDialog
          slug={slug}
          initial={deployTarget}
          open
          onOpenChange={(next) => setTargetDialog(next ? "edit" : null)}
        />
      ) : null}
      {targetDialog === "add" ? (
        <DeployTargetDialog
          slug={slug}
          presetEnvironment={environment.name}
          open
          onOpenChange={(next) => setTargetDialog(next ? "add" : null)}
        />
      ) : null}
      {showRedeploy && current && redeployEnvironmentId ? (
        <RollbackButton
          action="redeploy"
          slug={slug}
          environmentId={redeployEnvironmentId}
          environmentName={environment.name}
          version={current.version}
          open={redeployOpen}
          onOpenChange={setRedeployOpen}
          trigger={null}
        />
      ) : null}
      {showRemove ? (
        <RemoveEnvironment
          slug={slug}
          environmentId={removableEnvironmentId}
          environmentName={environment.name}
          open={removeOpen}
          onOpenChange={setRemoveOpen}
        />
      ) : null}
    </>
  );
}

// FrozenBanner is the card-level "this is stopped, and here's why" (#202). Amber
// (a hold, not a failure) matching the RolloutGatePrompt tone right below it.
//
// `freeze_reason` / `frozen_by` are REDACTED by the server for viewers, so their
// absence means "not allowed to see", never "not set" — the banner must still
// state the freeze clearly without them.
function FrozenBanner({ environment }: { environment: EnvironmentSummary }) {
  return (
    <div className="space-y-1 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs">
      <p className="flex items-center gap-1.5 font-medium text-amber-700 dark:text-amber-400">
        <Snowflake className="size-3.5" aria-hidden />
        Frozen — no deploy to this environment will be admitted
      </p>
      {environment.freeze_reason ? (
        <p className="text-muted-foreground">{environment.freeze_reason}</p>
      ) : null}
      {environment.frozen_at ? (
        <p className="text-muted-foreground">
          since <RelativeTime at={environment.frozen_at} fallback="—" />
          {environment.frozen_by ? ` by ${environment.frozen_by}` : null}
        </p>
      ) : null}
    </div>
  );
}

// FreezeControl is the maintainer toggle. Freezing opens a dialog (the reason is
// required); unfreezing is a single click — lifting a hold needs no extra input,
// and the server wakes the held runs immediately.
function FreezeControl({
  slug,
  environment,
}: {
  slug: string;
  environment: EnvironmentSummary;
}) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();

  if (!environment.frozen) {
    return (
      <FreezeDialog
        slug={slug}
        environment={environment.name}
        trigger={
          <Button variant="outline" size="sm" className="h-7 text-xs">
            <Snowflake className="mr-1 size-3.5" aria-hidden /> Freeze
          </Button>
        }
      />
    );
  }

  function unfreeze() {
    startTransition(async () => {
      const res = await unfreezeEnvironment({ slug, name: environment.name });
      if (!res.ok) {
        toast.error(`Unfreeze ${environment.name}: ${res.error}`);
        return;
      }
      toast.success(`${environment.name} unfrozen — held deploys resume`);
      router.refresh();
    });
  }

  return (
    <Button
      type="button"
      variant="outline"
      size="sm"
      className="h-7 text-xs"
      onClick={unfreeze}
      disabled={pending}
    >
      {pending ? (
        <Loader2 className="mr-1 size-3.5 animate-spin" aria-hidden />
      ) : (
        <Snowflake className="mr-1 size-3.5" aria-hidden />
      )}
      Unfreeze
    </Button>
  );
}

const PROVIDER_LABELS: Record<string, string> = { argocd: "ArgoCD" };

// The registered native provider target for this env. Maintainer-only (the parent
// only passes it when the maintainer-gated fetch succeeded). Config, not live state —
// live sync/degraded status lands in a later increment via a polled endpoint. Edit
// lives in the card actions menu so the target row stays informational.
function NativeTargetRow({ target }: { target: DeployTarget }) {
  const SyncIcon = target.sync_mode === "observe" ? Eye : RefreshCw;
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border border-border/60 bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
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
        <span className="min-w-0 break-all font-mono text-base font-semibold tracking-tight">
          {current.version}
        </span>
        {current.is_rollback ? <RollbackBadge /> : null}
      </div>
      <p className="text-xs leading-relaxed text-muted-foreground">
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
  currentRevisionId,
  onRetry,
}: {
  state: HistoryState;
  slug: string;
  environmentId: string;
  environmentName: string;
  currentRevisionId?: string;
  onRetry: () => Promise<void>;
}) {
  let body: ReactNode;

  if (state.phase === "loading") {
    body = (
      <p className="px-4 py-3 text-xs text-muted-foreground">
        Loading history…
      </p>
    );
  } else if (state.phase === "error") {
    body = (
      <div className="flex flex-wrap items-center justify-between gap-2 px-4 py-3 text-xs">
        <p className="text-red-500">
          Couldn&apos;t load history ({state.message}).
        </p>
        <Button
          type="button"
          variant="ghost"
          size="xs"
          onClick={() => void onRetry()}
        >
          Retry
        </Button>
      </div>
    );
  } else if (state.phase !== "loaded" || state.rows.length === 0) {
    body = (
      <p className="px-4 py-3 text-xs text-muted-foreground">
        No deploys recorded.
      </p>
    );
  } else {
    body = (
      <ol className="divide-y divide-border/60">
        {state.rows.map((d) => (
          <li
            key={d.id}
            className="flex items-center gap-3 px-4 py-2.5 text-xs hover:bg-muted/40"
          >
            <span className="flex min-w-0 items-center gap-2">
              <span
                className="min-w-0 truncate font-mono text-foreground"
                title={d.version}
              >
                {d.version}
              </span>
              {d.id === currentRevisionId ? (
                <span className="rounded-4xl bg-primary/10 px-1.5 py-0.5 text-[10px] font-medium text-primary">
                  current
                </span>
              ) : null}
              {d.is_rollback ? <RollbackBadge /> : null}
            </span>
            <span className="ml-auto flex shrink-0 items-center gap-2 text-muted-foreground">
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

  return (
    <div
      role="region"
      aria-label={`${environmentName} deployment history`}
      className="border-t border-border/60 bg-card"
    >
      <div data-history-scroll className={HISTORY_SCROLL_CLASS}>
        {body}
      </div>
      {state.phase === "loaded" && state.rows.length > 0 ? (
        <div className="flex items-center justify-between border-t border-border/60 bg-muted/40 px-4 py-2 text-xs text-muted-foreground">
          <span>
            Showing {state.rows.length} of {state.total} deployment
            {state.total === 1 ? "" : "s"}
          </span>
        </div>
      ) : null}
    </div>
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
