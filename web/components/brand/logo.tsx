import { cn } from "@/lib/utils";

// gocdnext mark — hexagon with rounded-arc corners framing a
// calligraphic `>` chevron. Three overlapping chevron paths: a
// filled brand-teal splash behind, a thin top arm, and a thick
// bottom tail — the stroke-weight contrast gives the mark its
// hand-drawn, heavier-on-the-bottom silhouette. Hexagon + two
// chevron strokes use currentColor so the parent's text colour
// paints them; the accent fill uses --primary so the brand teal
// survives dark mode without a prop dance.
//
// Usage:
//   <Logo className="size-5" />
//   <Logo wordmark className="size-6" />
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
      viewBox="0 0 160 160"
      width={size}
      height={size}
      fill="none"
      role="img"
      aria-label="gocdnext"
      className={cn("shrink-0", className)}
    >
      {/* Hexagon frame with quadratic-arc rounded corners. Uses
          currentColor so it follows the parent's text colour. */}
      <path
        d="M 64.41 21 Q 80 12 95.59 21 L 123.30 37 Q 138.89 46 138.89 64 L 138.89 96 Q 138.89 114 123.30 123 L 95.59 139 Q 80 148 64.41 139 L 36.70 123 Q 21.11 114 21.11 96 L 21.11 64 Q 21.11 46 36.70 37 Z"
        stroke="currentColor"
        strokeWidth="5"
      />

      {/* Brand-teal splash (the "fill" layer behind the stroked
          chevron). Offset centre (84,84) + scale 0.7 copied from
          the source design so the silhouette matches the mock. */}
      <g transform="translate(84 84) scale(0.7)">
        <path
          d="M -28 -38 Q -30 -44 -22 -42 L 22 -6 Q 30 0 22 6 L -22 42 Q -30 44 -28 38 Q -26 34 -20 30 L 14 0 L -20 -30 Q -26 -34 -28 -38 Z"
          fill="var(--primary)"
        />
      </g>

      {/* Two-weight stroked chevron — thin 3.5 on the top arm,
          thick 7 on the bottom tail. The weight contrast reads as
          a calligraphic "next" glyph when the eye lands on the
          mark. */}
      <g transform="translate(80 80) scale(0.7)">
        <path
          d="M -24 -36 Q -28 -40 -22 -40 L 18 -6 Q 26 0 18 6 L -14 34"
          stroke="currentColor"
          strokeWidth="3.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        <path
          d="M 22 4 L -22 42 Q -28 44 -26 38 L -16 28"
          stroke="currentColor"
          strokeWidth="7"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </g>
    </svg>
  );
}

export function Wordmark({ className }: { className?: string }) {
  // Bi-colour wordmark: "gocd" follows the parent text colour,
  // "next" paints brand teal via text-primary. Two spans so the
  // accent survives colour changes on the parent (e.g. when the
  // sidebar fades text-muted-foreground on hover, only "gocd"
  // desaturates — "next" stays branded).
  return (
    <span
      className={cn(
        "text-base font-bold tracking-tight leading-none",
        className,
      )}
    >
      gocd<span className="text-primary">next</span>
    </span>
  );
}
