import type { ComponentType, ReactNode } from "react";

import { cn } from "@/lib/utils";
import type { StatusTone } from "@/lib/status";

type Props = {
  tone: StatusTone;
  icon?: ComponentType<{ className?: string }>;
  children: ReactNode;
  className?: string;
};

// StatusPill is the canonical badge shape for any "colored label"
// across the app — webhook deliveries, integration state, agent
// health, dashboard quick callouts, etc. Takes a StatusTone plus
// optional leading icon; all colors come from design-system
// tokens so a palette refresh is a globals.css edit.
//
// Consumers map their domain-specific status to a tone at the
// edge:
//   <StatusPill tone={webhookTone(delivery.status)}>Accepted</StatusPill>
export function StatusPill({ tone, icon: Icon, children, className }: Props) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-xs font-medium",
        TONE[tone],
        className,
      )}
    >
      {Icon ? <Icon className="size-3.5" /> : null}
      {children}
    </span>
  );
}

// TONE is keyed on StatusTone — every value must have an entry so
// a missing key trips a compile error, not a visual bug.
const TONE: Record<StatusTone, string> = {
  success: "bg-status-success-bg text-status-success-fg",
  failed: "bg-status-failed-bg text-status-failed-fg",
  running: "bg-status-running-bg text-status-running-fg",
  queued: "bg-status-queued-bg text-status-queued-fg",
  canceled: "bg-status-canceled-bg text-status-canceled-fg",
  skipped: "bg-status-skipped-bg text-status-skipped-fg",
  warning: "bg-status-warning-bg text-status-warning-fg",
  neutral: "bg-muted text-muted-foreground",
  // Awaiting approval shares the warning palette — amber
  // reads as "attention needed" without the "failing" weight
  // of the destructive red. Operator eyes should land on
  // these pills first when scanning the run page.
  awaiting: "bg-status-warning-bg text-status-warning-fg",
};
