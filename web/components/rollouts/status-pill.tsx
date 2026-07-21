import { cn } from "@/lib/utils";
import { statusFor, TONE } from "@/lib/rollouts";

type Props = {
  phase: string;
  aborted: boolean;
  className?: string;
};

// StatusPill renders the rollout's aggregate phase as a tinted pill with a
// leading dot. Progressing pulses (teal); aborted always wins (red).
export function StatusPill({ phase, aborted, className }: Props) {
  const { label, tone } = statusFor(phase, aborted);
  return (
    <span
      className={cn(
        "inline-flex items-center gap-2 rounded-full border px-3 py-1 text-xs font-semibold",
        TONE[tone],
        className,
      )}
    >
      <span
        className={cn(
          "size-2 rounded-full bg-current",
          tone === "teal" ? "animate-pulse" : "",
        )}
        aria-hidden
      />
      {label}
    </span>
  );
}
