// Status helpers shared across run/stage/job displays. Kept in sync with the
// Go domain.RunStatus values (queued, running, success, failed, canceled,
// skipped, waiting). "success" matches the backend's terminal-pass label.

export type StatusVariant =
  | "default"
  | "secondary"
  | "destructive"
  | "outline"
  | "success";

export function statusVariant(status: string): StatusVariant {
  switch (status) {
    case "running":
      return "default";
    case "queued":
      return "secondary";
    case "success":
      return "success";
    case "failed":
      return "destructive";
    case "canceled":
    case "skipped":
    case "waiting":
      return "outline";
    default:
      return "outline";
  }
}

export function statusLabel(status: string): string {
  if (!status) return "";
  return status.charAt(0).toUpperCase() + status.slice(1);
}

export function isTerminalStatus(status: string): boolean {
  return (
    status === "success" ||
    status === "failed" ||
    status === "canceled" ||
    status === "skipped"
  );
}

// StatusTone is the semantic bucket that design-system components
// (StatusPill, StatusDot) consume. Every domain-specific status
// string (run/stage/job, webhook, agent health, metric threshold)
// maps onto one of these — there is no status value in the app
// that doesn't fit here.
export type StatusTone =
  | "success"
  | "failed"
  | "running"
  | "queued"
  | "canceled"
  | "skipped"
  | "warning"
  | "neutral";

// statusTone handles the run/stage/job vocabulary. Webhook and
// agent-health mappings live alongside their consumers (small and
// domain-specific enough that one central dispatcher adds noise).
export function statusTone(status: string): StatusTone {
  switch (status) {
    case "success":
    case "failed":
    case "running":
    case "queued":
    case "canceled":
    case "skipped":
      return status;
    case "waiting":
      return "queued";
    default:
      return "neutral";
  }
}
