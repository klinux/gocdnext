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
          <span style={{ fontSize: 48, fontWeight: 700, color: INK, letterSpacing: "-0.02em" }}>
            gocdnext
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
  return (
    <svg width="80" height="80" viewBox="0 0 32 32" fill={BRAND}>
      <line x1="9" y1="16" x2="13" y2="16" stroke={BRAND} strokeWidth="2" strokeLinecap="round" opacity="0.5" />
      <line x1="19" y1="16" x2="23" y2="16" stroke={BRAND} strokeWidth="2" strokeLinecap="round" opacity="0.5" />
      <circle cx="6" cy="16" r="3.5" opacity="0.65" />
      <circle cx="16" cy="16" r="3.5" />
      <circle cx="26" cy="16" r="3.5" opacity="0.85" />
    </svg>
  );
}
