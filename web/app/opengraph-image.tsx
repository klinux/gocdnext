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
  // Mirrors components/brand/logo.tsx exactly — rounded-arc
  // hexagon + 3-layer chevron (brand-teal fill behind, thin-top
  // + thick-bottom strokes over). Scaled to 100×100 in the OG.
  return (
    <svg width="100" height="100" viewBox="0 0 160 160" fill="none">
      <path
        d="M 64.41 21 Q 80 12 95.59 21 L 123.30 37 Q 138.89 46 138.89 64 L 138.89 96 Q 138.89 114 123.30 123 L 95.59 139 Q 80 148 64.41 139 L 36.70 123 Q 21.11 114 21.11 96 L 21.11 64 Q 21.11 46 36.70 37 Z"
        stroke={INK}
        strokeWidth="5"
      />
      <g transform="translate(84 84) scale(0.7)">
        <path
          d="M -28 -38 Q -30 -44 -22 -42 L 22 -6 Q 30 0 22 6 L -22 42 Q -30 44 -28 38 Q -26 34 -20 30 L 14 0 L -20 -30 Q -26 -34 -28 -38 Z"
          fill={BRAND}
        />
      </g>
      <g transform="translate(80 80) scale(0.7)">
        <path
          d="M -24 -36 Q -28 -40 -22 -40 L 18 -6 Q 26 0 18 6 L -14 34"
          stroke={INK}
          strokeWidth="3.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        <path
          d="M 22 4 L -22 42 Q -28 44 -26 38 L -16 28"
          stroke={INK}
          strokeWidth="7"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </g>
    </svg>
  );
}
