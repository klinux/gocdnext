import { cn } from "@/lib/utils";
import type { StatusTone } from "@/lib/status";

type Size = "sm" | "md" | "lg";

type Props = {
  tone: StatusTone;
  size?: Size;
  /** Pulse on running only by default; pass false to force static. */
  pulse?: boolean;
  className?: string;
  /** Accessible text replacement for screen-readers. */
  label?: string;
};

// StatusDot is the tiny colored circle used on list rows where a
// full StatusPill would be noisy: agents table, dashboard metric
// footer, sidebar hints, etc. Semantics come from the same
// StatusTone system so "the green dot" and "the green pill" can
// never drift out of sync.
export function StatusDot({ tone, size = "md", pulse, className, label }: Props) {
  const autoPulse = pulse ?? tone === "running";
  return (
    <span
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      className={cn(
        "inline-block shrink-0 rounded-full",
        SIZES[size],
        TONE[tone],
        autoPulse && "animate-pulse",
        className,
      )}
    />
  );
}

const SIZES: Record<Size, string> = {
  sm: "size-1.5",
  md: "size-2",
  lg: "size-2.5",
};

const TONE: Record<StatusTone, string> = {
  success: "bg-status-success",
  failed: "bg-status-failed",
  running: "bg-status-running",
  queued: "bg-status-queued",
  canceled: "bg-status-canceled",
  skipped: "bg-status-skipped",
  warning: "bg-status-warning",
  neutral: "bg-muted-foreground/40",
  // Shares the amber warning tone with StatusPill.awaiting so
  // the same visual weight carries across dot + pill views of
  // the same row.
  awaiting: "bg-status-warning",
};
