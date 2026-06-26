import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

// Shared service-tech vocabulary for the services cluster (pipelines list)
// and the run's Services detail cards. Services are environment containers
// (DB/cache/broker) — each tech gets a brand tint + a generic silhouette
// icon (from the services design handoff). The tech is detected from the
// service NAME/IMAGE; anything unrecognised falls back to a generic tile.

export type TechKey = "pg" | "redis" | "mongo" | "kafka" | "generic";

// Brand tints from the handoff (exact values — not theme tokens).
export const TECH: Record<TechKey, { bg: string; fg: string; icon: ReactNode }> = {
  pg: {
    bg: "rgba(51,103,145,.18)",
    fg: "#6aa5d6",
    icon: (
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} className="size-[60%]">
        <ellipse cx="12" cy="6" rx="7" ry="2.6" />
        <path d="M5 6v6c0 1.5 3.1 2.6 7 2.6s7-1.1 7-2.6V6M5 12v5c0 1.5 3.1 2.6 7 2.6s7-1.1 7-2.6v-5" strokeLinecap="round" />
      </svg>
    ),
  },
  redis: {
    bg: "rgba(220,56,44,.15)",
    fg: "#f2675c",
    icon: (
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} className="size-[60%]">
        <ellipse cx="12" cy="6.5" rx="7" ry="2.6" />
        <path d="M5 6.5v10c0 1.4 3.1 2.6 7 2.6s7-1.2 7-2.6v-10M5 11.5c0 1.4 3.1 2.6 7 2.6s7-1.2 7-2.6" strokeLinecap="round" />
      </svg>
    ),
  },
  mongo: {
    bg: "rgba(0,237,100,.12)",
    fg: "#4fd886",
    icon: (
      <svg viewBox="0 0 24 24" fill="currentColor" className="size-[60%]">
        <path d="M12 2c0 0 4.5 3.6 4.5 9 0 3.6-2.2 6.3-4.5 8-2.3-1.7-4.5-4.4-4.5-8 0-5.4 4.5-9 4.5-9z" />
      </svg>
    ),
  },
  kafka: {
    bg: "rgba(180,190,200,.12)",
    fg: "#c4ccd4",
    icon: (
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} className="size-[60%]">
        <circle cx="7" cy="6" r="2" />
        <circle cx="7" cy="18" r="2" />
        <circle cx="17" cy="12" r="2" />
        <path d="M8.8 6.7c3.6.7 2.7 4 5.4 4.5M8.8 17.3c3.6-.7 2.7-4 5.4-4.5" strokeLinecap="round" />
      </svg>
    ),
  },
  generic: {
    bg: "rgba(167,121,233,.14)",
    fg: "#b495e6",
    icon: (
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} className="size-[60%]">
        <ellipse cx="12" cy="6" rx="7" ry="2.6" />
        <path d="M5 6v12c0 1.5 3.1 2.6 7 2.6s7-1.1 7-2.6V6" strokeLinecap="round" />
      </svg>
    ),
  },
};

// detectTech maps a service name (or image) to a known tech for the
// tile tint/icon — name-based so the pipelines list needs no extra
// fetch; unknown → generic. Matches common aliases (postgresql, mongodb).
export function detectTech(nameOrImage: string): TechKey {
  const n = nameOrImage.toLowerCase();
  if (n.includes("postgres") || n.includes("pg")) return "pg";
  if (n.includes("redis")) return "redis";
  if (n.includes("mongo")) return "mongo";
  if (n.includes("kafka")) return "kafka";
  return "generic";
}

// ServiceGlyph is the brand-tinted rounded square with the tech icon.
// Size + corner via className (the cluster uses 22px tiles; the detail
// card uses a larger logo).
export function ServiceGlyph({
  tech,
  className,
}: {
  tech: TechKey;
  className?: string;
}) {
  const t = TECH[tech];
  return (
    <span
      className={cn("flex items-center justify-center rounded-md", className)}
      style={{ background: t.bg, color: t.fg }}
    >
      {t.icon}
    </span>
  );
}
