"use client";

import {
  AlertTriangle,
  CheckCircle2,
  GitCommitHorizontal,
  PauseCircle,
  RefreshCw,
} from "lucide-react";

import { RelativeTime } from "@/components/shared/relative-time";
import type { DeployWatch } from "@/types/api";

function shortRev(rev: string): string {
  return rev.length > 7 ? rev.slice(0, 7) : rev;
}

// stepLabel renders "step N/M" for a canary with steps. The controller index is
// 0-based, shown 1-based (clamped to the count for the brief completion tick). An
// UNKNOWN index (rollout_current_step absent — distinct from step 0) shows "?".
function stepLabel(w: DeployWatch): string | null {
  const count = w.rollout_step_count ?? 0;
  if (count === 0) return null; // no canary steps (e.g. blue-green)
  if (w.rollout_current_step === undefined) return `step ?/${count}`;
  return `step ${Math.min(w.rollout_current_step + 1, count)}/${count}`;
}

// Chip base class + the design's status-pill tones (paused=amber, progressing=teal,
// degraded/aborted=red, healthy=green).
const CHIP =
  "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-medium";
const TONE = {
  amber: "border-amber-500/40 bg-amber-500/10 text-amber-600 dark:text-amber-400",
  teal: "border-teal-500/40 bg-teal-500/10 text-teal-600 dark:text-teal-400",
  red: "border-red-500/40 bg-red-500/10 text-red-600 dark:text-red-400",
  green:
    "border-emerald-500/40 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400",
  sky: "border-sky-500/40 bg-sky-500/10 text-sky-600 dark:text-sky-400",
  neutral: "border-border bg-muted text-muted-foreground",
} as const;

// AnchorBadge shows the correlation revision — the commit ArgoCD must report before
// this deploy can succeed. It rides alongside the state chip (like AnalysisBadge)
// instead of living inside it, because the rollout and gate states replace the generic
// chip's text and would otherwise hide the anchor exactly when it matters: a deploy
// that stalls to its deadline is nearly always waiting on a revision that never
// arrives, and the version LABEL alone no longer reveals which commit that is.
function AnchorBadge({ watch }: { watch: DeployWatch }) {
  const rev = shortRev(watch.expected_revision);
  if (!rev) return null;
  return (
    <span
      className={`${CHIP} ${TONE.neutral}`}
      title={`Waiting for ArgoCD to report revision ${watch.expected_revision}`}
    >
      <GitCommitHorizontal className="size-3" aria-hidden />
      <span className="font-mono font-normal">{rev}</span>
    </span>
  );
}

// RolloutChip is the read-only canary/blue-green progress (Phase 2). No control here.
function RolloutChip({ watch }: { watch: DeployWatch }) {
  if (watch.rollout_error) {
    return (
      <span className={`${CHIP} ${TONE.amber}`} title={watch.rollout_error}>
        <AlertTriangle className="size-3" aria-hidden />
        Rollout status unavailable
      </span>
    );
  }
  const step = stepLabel(watch);
  const meta = step ? <span className="font-mono font-normal">{step}</span> : null;

  if (watch.rollout_aborted) {
    return (
      <span className={`${CHIP} ${TONE.red}`}>
        <AlertTriangle className="size-3" aria-hidden /> Rollout aborted
      </span>
    );
  }
  switch (watch.rollout_phase) {
    case "Paused":
      return (
        <span className={`${CHIP} ${TONE.amber}`}>
          <PauseCircle className="size-3" aria-hidden /> Canary paused {meta}
        </span>
      );
    case "Degraded":
      return (
        <span className={`${CHIP} ${TONE.red}`}>
          <AlertTriangle className="size-3" aria-hidden /> Rollout degraded {meta}
        </span>
      );
    case "Healthy":
      return (
        <span className={`${CHIP} ${TONE.green}`}>
          <CheckCircle2 className="size-3" aria-hidden /> Rollout healthy
        </span>
      );
    default: // Progressing / unknown
      return (
        <span className={`${CHIP} ${TONE.teal}`}>
          <RefreshCw className="size-3 animate-spin" aria-hidden /> Rolling out {meta}
        </span>
      );
  }
}

// analysisTone maps an AnalysisRun phase to a status tone (failed/error=red,
// inconclusive=amber, successful=green, running/pending/unknown=teal).
function analysisTone(phase: string): keyof typeof TONE {
  switch (phase) {
    case "Failed":
    case "Error":
      return "red";
    case "Inconclusive":
      return "amber";
    case "Successful":
      return "green";
    default:
      return "teal";
  }
}

// AnalysisBadge surfaces the active metric-analysis run (observe-only, Phase 2c) — WHY a
// canary paused/degraded. The (bounded) cluster message is a hover title.
function AnalysisBadge({ watch }: { watch: DeployWatch }) {
  const phase = watch.rollout_analysis_phase;
  if (!phase) return null;
  return (
    <span
      className={`${CHIP} ${TONE[analysisTone(phase)]}`}
      title={watch.rollout_analysis_message || undefined}
    >
      analysis {phase.toLowerCase()}
    </span>
  );
}

// NativeWatchChip renders the live state of an in-flight native deploy. A rollout-aware
// deploy that has been observed shows its canary/blue-green progress; otherwise the
// Application-level state (Degraded wins over Syncing; pre-sync reads "Deploying").
export function NativeWatchChip({ watch }: { watch: DeployWatch }) {
  if (watch.degraded_since) {
    return (
      <>
        <span className={`${CHIP} ${TONE.amber}`}>
          <AlertTriangle className="size-3" aria-hidden />
          Degraded <RelativeTime at={watch.degraded_since} />
        </span>
        <AnchorBadge watch={watch} />
      </>
    );
  }
  // Rollout progress once observed (a phase or a read error). Before the first
  // observation it falls through to the generic Application chip below. A running
  // AnalysisRun rides alongside as a second badge.
  if (watch.rollout_aware && (watch.rollout_phase || watch.rollout_error)) {
    return (
      <>
        <RolloutChip watch={watch} />
        <AnalysisBadge watch={watch} />
        <AnchorBadge watch={watch} />
      </>
    );
  }
  const rev = shortRev(watch.expected_revision);
  return (
    <span className={`${CHIP} ${TONE.sky}`} title={rev ? `Waiting for ArgoCD to report revision ${watch.expected_revision}` : undefined}>
      <RefreshCw className="size-3 animate-spin" aria-hidden />
      {watch.sync_requested_at ? "Syncing" : "Deploying"}
      {rev ? <span className="font-mono font-normal">{rev}</span> : null}
    </span>
  );
}
