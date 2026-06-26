import type { ReactNode } from "react";

import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import type { StatusTone } from "@/lib/status";

// ServicesCluster renders a pipeline's declared services as a violet
// bracket of per-tech tiles before the stage track — services are
// environment containers (DB/cache/broker), not jobs: they boot before
// the jobs and stay alive for the whole run, so they get their own colour
// (violet) distinct from jobs/stages (teal). Recreated from the services
// design handoff.
//
// The tech is detected from the service NAME (no extra data fetch) — a
// generic tile covers anything unrecognised. The readiness dot is an
// approximation derived from the run tone (the precise, per-service
// ready/booting/failed lives in the run's Services detail); the list
// stays fetch-free.

type TechKey = "pg" | "redis" | "mongo" | "kafka" | "generic";

// Brand tints from the handoff (exact values — not theme tokens).
const TECH: Record<TechKey, { bg: string; fg: string; icon: ReactNode }> = {
  pg: {
    bg: "rgba(51,103,145,.18)",
    fg: "#6aa5d6",
    icon: (
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} className="size-[13px]">
        <ellipse cx="12" cy="6" rx="7" ry="2.6" />
        <path d="M5 6v6c0 1.5 3.1 2.6 7 2.6s7-1.1 7-2.6V6M5 12v5c0 1.5 3.1 2.6 7 2.6s7-1.1 7-2.6v-5" strokeLinecap="round" />
      </svg>
    ),
  },
  redis: {
    bg: "rgba(220,56,44,.15)",
    fg: "#f2675c",
    icon: (
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} className="size-[13px]">
        <ellipse cx="12" cy="6.5" rx="7" ry="2.6" />
        <path d="M5 6.5v10c0 1.4 3.1 2.6 7 2.6s7-1.2 7-2.6v-10M5 11.5c0 1.4 3.1 2.6 7 2.6s7-1.2 7-2.6" strokeLinecap="round" />
      </svg>
    ),
  },
  mongo: {
    bg: "rgba(0,237,100,.12)",
    fg: "#4fd886",
    icon: (
      <svg viewBox="0 0 24 24" fill="currentColor" className="size-[13px]">
        <path d="M12 2c0 0 4.5 3.6 4.5 9 0 3.6-2.2 6.3-4.5 8-2.3-1.7-4.5-4.4-4.5-8 0-5.4 4.5-9 4.5-9z" />
      </svg>
    ),
  },
  kafka: {
    bg: "rgba(180,190,200,.12)",
    fg: "#c4ccd4",
    icon: (
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} className="size-[13px]">
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
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} className="size-[13px]">
        <ellipse cx="12" cy="6" rx="7" ry="2.6" />
        <path d="M5 6v12c0 1.5 3.1 2.6 7 2.6s7-1.1 7-2.6V6" strokeLinecap="round" />
      </svg>
    ),
  },
};

// detectTech maps a service name to a known tech for the tile tint/icon.
// Name-based (the list already has names) so no extra fetch; unknown →
// generic. Matches common aliases (postgresql, mongodb).
function detectTech(name: string): TechKey {
  const n = name.toLowerCase();
  if (n.includes("postgres") || n.includes("pg")) return "pg";
  if (n.includes("redis")) return "redis";
  if (n.includes("mongo")) return "mongo";
  if (n.includes("kafka")) return "kafka";
  return "generic";
}

// dotColor approximates per-service readiness from the run tone (the list
// is fetch-free): a finished/running run → ready, a waiting run → booting,
// a failed run → failed, otherwise muted. Precise per-service state is in
// the run detail.
function dotColor(tone: StatusTone): string {
  switch (tone) {
    case "success":
    case "running":
      return "#3fb950"; // ready
    case "queued":
    case "awaiting":
      return "#d9a429"; // booting
    case "failed":
      return "#f85149"; // failed
    default:
      return "#6e7681"; // muted (canceled/skipped/warning/neutral)
  }
}

function Tile({ tech, dot }: { tech: TechKey; dot: string }) {
  const t = TECH[tech];
  return (
    <span
      className="relative flex size-[22px] items-center justify-center rounded-md"
      style={{ background: t.bg, color: t.fg }}
    >
      {t.icon}
      <span
        className="absolute -bottom-px -right-px size-[6px] rounded-full ring-2 ring-card"
        style={{ background: dot }}
        aria-hidden
      />
    </span>
  );
}

// Cap the tiles so the cluster + connector stay inside the fixed lane
// (keeps the stage circles aligned); overflow collapses to "+N".
const MAX_TILES = 3;

export function ServicesCluster({
  names,
  tone,
}: {
  names: string[];
  tone: StatusTone;
}) {
  if (names.length === 0) return null;
  const dot = dotColor(tone);
  const shown = names.slice(0, MAX_TILES);
  const extra = names.length - shown.length;
  return (
    <div className="flex items-start gap-0">
      <Tooltip>
        <TooltipTrigger
          render={<div className="flex cursor-help flex-col items-center gap-1" />}
        >
          <div
            className="flex items-center gap-[3px] rounded-[9px] border px-[7px] py-1"
            style={{
              background: "rgba(167,121,233,.09)",
              borderColor: "rgba(167,121,233,.32)",
            }}
          >
            {shown.map((n, i) => (
              <Tile key={`${n}-${i}`} tech={detectTech(n)} dot={dot} />
            ))}
            {extra > 0 ? (
              <span
                className="px-0.5 font-mono text-[9px] font-semibold"
                style={{ color: "#9d7fd0" }}
              >
                +{extra}
              </span>
            ) : null}
          </div>
          <span
            className="font-mono text-[8.5px] font-semibold uppercase tracking-wide"
            style={{ color: "#9d7fd0" }}
          >
            services
          </span>
        </TooltipTrigger>
        <TooltipContent>
          Services (environment): {names.join(", ")} — start before the jobs
          and stay alive for the whole run
        </TooltipContent>
      </Tooltip>
      {/* dashed violet connector linking the cluster to the first job */}
      <span
        className="mt-[10px] ml-1 w-4 border-t border-dashed"
        style={{ borderColor: "rgba(167,121,233,.4)" }}
        aria-hidden
      />
    </div>
  );
}
