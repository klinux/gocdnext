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
