import { useId } from "react";

import { cn } from "@/lib/utils";

// gocdnext mark — hexagon frame wrapping two gradient-filled
// organic curves (upper teal-highlight + lower teal-depth), the
// "ribbon inside a cell" silhouette Kleber finalised on
// 2026-04-24 (see app/icon.svg). Hexagon stroke uses
// currentColor so parents (sidebar, headings, login header)
// control its tone; the gradients stay fixed because they're
// the brand identity.
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
  // useId gives stable-across-render IDs unique per component
  // instance so two <Logo /> on the same page don't collide on
  // `url(#pbTop)` — important once breadcrumbs or dialogs might
  // render a second instance alongside the sidebar.
  const rawId = useId();
  // `:` from useId isn't legal in an SVG fragment id; strip it.
  const idBase = rawId.replace(/:/g, "");
  const topId = `${idBase}-top`;
  const tealId = `${idBase}-teal`;

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
      <defs>
        <linearGradient
          id={topId}
          x1="23.796248"
          y1="62.783008"
          x2="85.930894"
          y2="124.91765"
          gradientTransform="matrix(1.2400315,0.08753382,-0.03824616,0.54180708,17.388739,9.4616189)"
          gradientUnits="userSpaceOnUse"
        >
          <stop offset="0%" stopColor="#8FE4EA" />
          <stop offset="55%" stopColor="#0ea5b5" />
          <stop offset="100%" stopColor="#086370" />
        </linearGradient>
        <linearGradient
          id={tealId}
          x1="27.074875"
          y1="121.03715"
          x2="85.930894"
          y2="179.27196"
          gradientTransform="matrix(1.21055,-0.09023533,0.03942653,0.52892569,10.577888,22.499414)"
          gradientUnits="userSpaceOnUse"
        >
          <stop offset="0%" stopColor="#4FD1DB" />
          <stop offset="100%" stopColor="#086370" />
        </linearGradient>
      </defs>

      {/* Hexagon frame with quadratic-arc rounded corners. Stroke
          is currentColor so the parent's text colour controls it;
          the faint fill-opacity lifts the interior a hair above
          pure transparent so the mark still reads on busy
          backgrounds without swallowing the gradients. */}
      <path
        d="M 64.41 21 Q 80 12 95.59 21 L 123.30 37 Q 138.89 46 138.89 64 L 138.89 96 Q 138.89 114 123.30 123 L 95.59 139 Q 80 148 64.41 139 L 36.70 123 Q 21.11 114 21.11 96 L 21.11 64 Q 21.11 46 36.70 37 Z"
        stroke="currentColor"
        strokeWidth={5}
        fill="currentColor"
        fillOpacity={0.05}
      />

      {/* Lower ribbon — deeper teal gradient, denser weight. */}
      <path
        d="M 46.184096,111.57288 C 58.397265,117.09952 75.308755,113.69325 96.918564,101.35407 C 111.29861,92.772312 118.82326,85.774394 119.49252,80.360321 C 107.43841,76.967495 92.461909,77.547436 74.563036,82.100143 C 56.743694,87.71976 60.633741,97.151369 46.184096,111.57288 Z"
        fill={`url(#${tealId})`}
      />

      {/* Upper ribbon — lighter teal highlight. */}
      <path
        d="M 44.235256,49.249387 C 56.719957,43.540652 74.051942,46.960793 96.231212,59.50981 C 110.99169,68.24012 118.72549,75.37608 119.43264,80.91769 C 107.10224,84.44064 91.763168,83.907024 73.415437,79.316842 C 62.408657,75.596959 45.418126,63.611284 44.235256,49.249387 Z"
        fill={`url(#${topId})`}
      />
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
