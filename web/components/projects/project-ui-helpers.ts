import type { StatusTone } from "@/lib/status";
import type { ProjectProvider, ProjectStatus } from "@/types/api";

export function countBy<T, K extends string>(
  items: T[],
  keyFn: (item: T) => K,
): Record<K, number> {
  const out = {} as Record<K, number>;
  for (const item of items) {
    const k = keyFn(item);
    out[k] = (out[k] ?? 0) + 1;
  }
  return out;
}

export function statusLabel(s: ProjectStatus): string {
  switch (s) {
    case "no_pipelines":
      return "No pipelines";
    case "never_run":
      return "Never run";
    case "running":
      return "Running";
    case "failing":
      return "Failing";
    case "success":
      return "Healthy";
  }
}

export function statusToTone(s: ProjectStatus): StatusTone {
  switch (s) {
    case "running":
      return "running";
    case "success":
      return "success";
    case "failing":
      return "failed";
    case "never_run":
    case "no_pipelines":
      return "neutral";
  }
}

export function providerLabel(p: ProjectProvider): string {
  if (!p) return "No repo";
  return p.charAt(0).toUpperCase() + p.slice(1);
}

// Shared style tables — card-status pills, dots, active filter pill
// variants. Pulled out so both grid and list views reach for the
// same tokens and stay in sync visually.
export const tonePillClasses: Record<StatusTone, string> = {
  success:
    "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
  failed: "border-red-500/30 bg-red-500/10 text-red-700 dark:text-red-400",
  running: "border-sky-500/30 bg-sky-500/10 text-sky-700 dark:text-sky-400",
  queued:
    "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  warning:
    "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  awaiting:
    "border-amber-500/50 bg-amber-500/15 text-amber-700 dark:text-amber-400",
  canceled: "border-muted-foreground/30 bg-muted text-muted-foreground",
  skipped: "border-muted-foreground/20 bg-muted/50 text-muted-foreground",
  neutral: "border-border bg-muted/40 text-muted-foreground",
};

export const toneDotClasses: Record<StatusTone, string> = {
  success: "bg-emerald-500",
  failed: "bg-red-500",
  running: "bg-sky-500",
  queued: "bg-amber-500",
  warning: "bg-amber-500",
  awaiting: "bg-amber-500",
  canceled: "bg-muted-foreground",
  skipped: "bg-muted-foreground/60",
  neutral: "bg-muted-foreground/40",
};

export const activePillClasses: Record<"all" | StatusTone, string> = {
  all: "border-primary bg-primary text-primary-foreground",
  success:
    "border-emerald-500/50 bg-emerald-500/15 text-emerald-700 dark:text-emerald-400",
  failed: "border-red-500/50 bg-red-500/15 text-red-700 dark:text-red-400",
  running: "border-sky-500/50 bg-sky-500/15 text-sky-700 dark:text-sky-400",
  queued:
    "border-amber-500/50 bg-amber-500/15 text-amber-700 dark:text-amber-400",
  warning:
    "border-amber-500/50 bg-amber-500/15 text-amber-700 dark:text-amber-400",
  awaiting:
    "border-amber-500/60 bg-amber-500/20 text-amber-700 dark:text-amber-400",
  canceled: "border-foreground/40 bg-muted text-foreground",
  skipped: "border-foreground/30 bg-muted text-foreground",
  neutral: "border-foreground/40 bg-muted text-foreground",
};
