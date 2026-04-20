import { cn } from "@/lib/utils";

// gocdnext mark — three stages connected by pipeline lines.
// SVG uses currentColor throughout so a single <Logo /> call
// recolors just by changing text-* on the parent. The shape
// reads at 16×16 (favicon) and scales cleanly.
//
// Usage:
//   <Logo className="size-5 text-primary" />
//   <Logo wordmark className="size-6 text-foreground" />
type LogoProps = {
  className?: string;
  /** Render the "gocdnext" wordmark next to the mark. */
  wordmark?: boolean;
  /** Size of the mark, in pixels; wordmark font follows parent. */
  size?: number;
};

export function Logo({ className, wordmark, size = 24 }: LogoProps) {
  if (wordmark) {
    return (
      <span className={cn("inline-flex items-center gap-2", className)}>
        <Mark size={size} />
        <Wordmark />
      </span>
    );
  }
  return <Mark size={size} className={className} />;
}

function Mark({ size, className }: { size: number; className?: string }) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 32 32"
      width={size}
      height={size}
      fill="none"
      role="img"
      aria-label="gocdnext"
      className={cn("shrink-0", className)}
    >
      {/* Connectors — the "pipeline" between stages. Subtle so
          the filled stage circles are the focal point. */}
      <line
        x1="9"
        y1="16"
        x2="13"
        y2="16"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        opacity="0.5"
      />
      <line
        x1="19"
        y1="16"
        x2="23"
        y2="16"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        opacity="0.5"
      />
      {/* Three stages — left slightly lighter, right darker
          creates a subtle left-to-right flow cue. */}
      <circle cx="6" cy="16" r="3.5" fill="currentColor" opacity="0.65" />
      <circle cx="16" cy="16" r="3.5" fill="currentColor" />
      <circle cx="26" cy="16" r="3.5" fill="currentColor" opacity="0.85" />
    </svg>
  );
}

export function Wordmark({ className }: { className?: string }) {
  // Plain text in the system font with tight tracking. No custom
  // SVG wordmark because it would duplicate the font for little
  // gain and fight accessibility (screen readers + copy-paste).
  return (
    <span
      className={cn(
        "text-sm font-semibold tracking-tight leading-none",
        className,
      )}
    >
      gocdnext
    </span>
  );
}
