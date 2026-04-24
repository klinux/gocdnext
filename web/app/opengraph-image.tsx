import { ImageResponse } from "next/og";

// Route-level OG image. Next.js 15 picks this up automatically
// and serves it at /opengraph-image on every page. Rendered by
// @vercel/og (Satori) — so no CSS vars, no Tailwind tokens; we
// hardcode the brand hex (mirror of --brand-500) and keep the
// layout to what Satori can layout reliably.

export const runtime = "edge";
export const alt = "gocdnext — modern CI/CD orchestrator";
export const size = { width: 1200, height: 630 };
export const contentType = "image/png";

const BRAND = "#0ea5b5"; // oklch(0.65 0.14 195) ~= this in sRGB
const INK = "#0b1220";
const MUTED = "#64748b";

export default function OGImage() {
  return new ImageResponse(
    (
      <div
        style={{
          width: "100%",
          height: "100%",
          display: "flex",
          flexDirection: "column",
          justifyContent: "space-between",
          padding: "72px",
          background: `linear-gradient(135deg, #ffffff 0%, #f1fafd 100%)`,
          fontFamily: "sans-serif",
        }}
      >
        <div style={{ display: "flex", alignItems: "center", gap: 20 }}>
          <Mark />
          <span style={{ fontSize: 48, fontWeight: 700, color: INK, letterSpacing: "-0.02em", display: "flex" }}>
            gocd<span style={{ color: BRAND }}>next</span>
          </span>
        </div>

        <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
          <p
            style={{
              fontSize: 72,
              fontWeight: 600,
              color: INK,
              letterSpacing: "-0.03em",
              lineHeight: 1.05,
              margin: 0,
            }}
          >
            Modern CI/CD,
            <br />
            built as a pipeline.
          </p>
          <p style={{ fontSize: 28, color: MUTED, margin: 0 }}>
            Stages, jobs, secrets, artifacts — on your own agents.
          </p>
        </div>

        <div style={{ display: "flex", gap: 12, alignItems: "center" }}>
          <span
            style={{
              height: 4,
              width: 120,
              background: BRAND,
              borderRadius: 2,
            }}
          />
          <span style={{ fontSize: 22, color: MUTED }}>github.com/klinux/gocdnext</span>
        </div>
      </div>
    ),
    { ...size },
  );
}

function Mark() {
  // Inline SVG so Satori renders it without needing a font asset.
  // Mirrors components/brand/logo.tsx — hexagon frame + two
  // gradient-filled ribbon curves. Satori supports <linearGradient>
  // with numeric stops, so the same paths used in the favicon
  // land here too. Scaled to 100×100 in the OG header.
  return (
    <svg width="100" height="100" viewBox="0 0 160 160" fill="none">
      <defs>
        <linearGradient
          id="ogTop"
          x1="23.796248"
          y1="62.783008"
          x2="85.930894"
          y2="124.91765"
          gradientTransform="matrix(1.2400315,0.08753382,-0.03824616,0.54180708,17.388739,9.4616189)"
          gradientUnits="userSpaceOnUse"
        >
          <stop offset="0%" stopColor="#8FE4EA" />
          <stop offset="55%" stopColor={BRAND} />
          <stop offset="100%" stopColor="#086370" />
        </linearGradient>
        <linearGradient
          id="ogTeal"
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
      <path
        d="M 64.41 21 Q 80 12 95.59 21 L 123.30 37 Q 138.89 46 138.89 64 L 138.89 96 Q 138.89 114 123.30 123 L 95.59 139 Q 80 148 64.41 139 L 36.70 123 Q 21.11 114 21.11 96 L 21.11 64 Q 21.11 46 36.70 37 Z"
        stroke={INK}
        strokeWidth="5"
      />
      <path
        d="M 46.184096,111.57288 C 58.397265,117.09952 75.308755,113.69325 96.918564,101.35407 C 111.29861,92.772312 118.82326,85.774394 119.49252,80.360321 C 107.43841,76.967495 92.461909,77.547436 74.563036,82.100143 C 56.743694,87.71976 60.633741,97.151369 46.184096,111.57288 Z"
        fill="url(#ogTeal)"
      />
      <path
        d="M 44.235256,49.249387 C 56.719957,43.540652 74.051942,46.960793 96.231212,59.50981 C 110.99169,68.24012 118.72549,75.37608 119.43264,80.91769 C 107.10224,84.44064 91.763168,83.907024 73.415437,79.316842 C 62.408657,75.596959 45.418126,63.611284 44.235256,49.249387 Z"
        fill="url(#ogTop)"
      />
    </svg>
  );
}
