// Pure rendering logic for the Rollouts dashboard (ADR-0001, PR-B). Kept
// separate from the components so the branching (step state, traffic split,
// status/analysis tones) is unit-testable without a DOM. No I/O here.

import type { RolloutStep } from "@/types/api";

// Status/analysis tones reuse the app-wide semantic palette (same tone classes
// the Environments card uses in native-watch-chip.client.tsx): paused=amber,
// progressing=teal, healthy=green, degraded/aborted=red. Theme-aware via the
// Tailwind color scales (they resolve light/dark automatically).
export type RolloutTone = "teal" | "amber" | "green" | "red" | "sky" | "neutral";

export const TONE: Record<RolloutTone, string> = {
  teal: "border-teal-500/40 bg-teal-500/10 text-teal-600 dark:text-teal-400",
  amber:
    "border-amber-500/40 bg-amber-500/10 text-amber-600 dark:text-amber-400",
  green:
    "border-emerald-500/40 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400",
  red: "border-red-500/40 bg-red-500/10 text-red-600 dark:text-red-400",
  sky: "border-sky-500/40 bg-sky-500/10 text-sky-600 dark:text-sky-400",
  neutral: "border-border bg-muted text-muted-foreground",
} as const;

// statusFor maps a rollout's phase (+ aborted flag) to a display label + tone.
// Aborted wins over the reported phase: the controller may still say
// "Degraded"/"Paused" while the traffic has already snapped back to stable.
export function statusFor(
  phase: string,
  aborted: boolean,
): { label: string; tone: RolloutTone } {
  if (aborted) return { label: "Aborted", tone: "red" };
  switch (phase) {
    case "Paused":
      return { label: "Paused", tone: "amber" };
    case "Progressing":
      return { label: "Progressing", tone: "teal" };
    case "Healthy":
      return { label: "Healthy", tone: "green" };
    case "Degraded":
      return { label: "Degraded", tone: "red" };
    default:
      return { label: phase || "Unknown", tone: "neutral" };
  }
}

export type StepState = "done" | "current" | "pending";

// stepState decides a node's visual state. When the controller hasn't reported
// the current index (known=false) or the rollout was aborted, we can't trust
// any progress, so every node reads "pending" and nothing is highlighted. When
// the index runs past the last step (fully promoted) every node reads "done".
export function stepState(
  index: number,
  currentIndex: number,
  known: boolean,
  aborted: boolean,
): StepState {
  if (!known || aborted) return "pending";
  if (index < currentIndex) return "done";
  if (index === currentIndex) return "current";
  return "pending";
}

// isManualGate marks the indefinite pause (`pause: {}`, no duration) — the human
// approval gate. Rendered as an amber "manual" node, distinct from a timed pause.
export function isManualGate(step: RolloutStep): boolean {
  return step.kind === "pause" && step.pause_duration === "";
}

// stepLabel is the value line under a node.
export function stepLabel(step: RolloutStep): string {
  switch (step.kind) {
    case "setWeight":
      return step.weight != null ? `${step.weight}%` : "setWeight";
    case "pause":
      return step.pause_duration === "" ? "manual" : step.pause_duration;
    case "setCanaryScale":
      return step.weight != null ? `scale ${step.weight}%` : "scale";
    case "analysis":
      return "analysis";
    case "experiment":
      return "experiment";
    case "plugin":
      return "plugin";
    default:
      return step.kind || "step";
  }
}

// trafficSplit clamps canary_weight to [0,100] and derives the stable share. A
// non-finite weight (NaN from a malformed payload) is treated as 0% canary.
export function trafficSplit(canaryWeight: number): {
  canary: number;
  stable: number;
} {
  const c = Number.isFinite(canaryWeight)
    ? Math.min(100, Math.max(0, Math.round(canaryWeight)))
    : 0;
  return { canary: c, stable: 100 - c };
}

// analysisTone maps an AnalysisRun phase to a tone (mirrors the deploy chip's
// analysisTone): failed/error=red, inconclusive=amber, successful=green, and
// running/pending/unknown=teal.
export function analysisTone(phase: string): RolloutTone {
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

// shortHash trims a ReplicaSet pod-template-hash for display.
export function shortHash(h: string): string {
  return h.length > 10 ? h.slice(0, 10) : h;
}

// imageParts splits "repo/name:tag" into name + tag for two-tone rendering. A
// digest ref (…@sha256:…) or a tagless image keeps the whole string as the
// name — the ':' inside "sha256:" must never be mistaken for a tag separator.
export function imageParts(image: string): { name: string; tag?: string } {
  if (!image) return { name: "" };
  if (image.includes("@")) return { name: image };
  const slash = image.lastIndexOf("/");
  const colon = image.lastIndexOf(":");
  if (colon > slash) {
    return { name: image.slice(0, colon), tag: image.slice(colon + 1) };
  }
  return { name: image };
}
