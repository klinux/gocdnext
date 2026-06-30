import type { StatusTone } from "@/lib/status";

// Security finding severities, worst → least.
export type Severity = "critical" | "high" | "medium" | "low";

export const SEVERITY_ORDER: Severity[] = ["critical", "high", "medium", "low"];

// Map a finding severity onto a design-system status tone (reused by StatusPill)
// so severity badges share the app's palette. Unknown → neutral.
const TONE: Record<Severity, StatusTone> = {
  critical: "failed",
  high: "warning",
  medium: "running",
  low: "queued",
};

export function severityTone(severity: string): StatusTone {
  return TONE[severity as Severity] ?? "neutral";
}

export function severityLabel(severity: string): string {
  if (!severity) return "—";
  return severity.charAt(0).toUpperCase() + severity.slice(1);
}
