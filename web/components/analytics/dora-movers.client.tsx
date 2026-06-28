import { Eye, TrendingDown, TrendingUp } from "lucide-react";

import { cn } from "@/lib/utils";
import type { DoraOverview } from "@/server/queries/analytics";

import { type Mover, computeMovers } from "./dora-movers";

const STYLE: Record<
  Mover["kind"],
  { label: string; tone: string; icon: typeof TrendingUp }
> = {
  up: { label: "Biggest improvement", tone: "text-status-success", icon: TrendingUp },
  down: { label: "Biggest regression", tone: "text-status-failed", icon: TrendingDown },
  watch: { label: "Watch", tone: "text-status-warning", icon: Eye },
};

// DoraMovers renders the window's highlights — biggest improvement, biggest
// regression, and a watch item — derived from current vs prior per-group
// rollups. Renders nothing when no group qualifies.
export function DoraMovers({
  overview,
  windowDays,
}: {
  overview: DoraOverview;
  windowDays: number;
}) {
  const movers = computeMovers(overview.teams, overview.teams_prior, windowDays);
  if (movers.length === 0) return null;

  return (
    <div className="grid gap-3.5 sm:grid-cols-3">
      {movers.map((m) => {
        const s = STYLE[m.kind];
        const Icon = s.icon;
        return (
          <div key={m.kind} className="rounded-xl bg-card p-4 ring-1 ring-foreground/10">
            <div
              className={cn(
                "flex items-center gap-1.5 font-mono text-[11px] font-semibold uppercase tracking-wide",
                s.tone,
              )}
            >
              <Icon className="size-3.5" aria-hidden />
              {s.label}
            </div>
            <p className="mt-2 text-sm text-foreground">
              <span className="font-mono font-semibold text-brand-500">{m.team}</span>{" "}
              {m.text}
            </p>
            <p className="mt-1.5 text-xs text-muted-foreground/70">{m.foot}</p>
          </div>
        );
      })}
    </div>
  );
}
