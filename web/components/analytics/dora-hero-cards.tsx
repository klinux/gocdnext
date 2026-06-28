import {
  ArrowDown,
  ArrowUp,
  Rocket,
  RotateCcw,
  Timer,
  TriangleAlert,
} from "lucide-react";

import { Card } from "@/components/ui/card";
import { type Delta, TIER_BG, TIER_COLOR, TIER_LABEL, type Tier } from "@/lib/dora";
import { cn } from "@/lib/utils";
import type { DoraOverview } from "@/server/queries/analytics";

import { DoraSparkline } from "./dora-sparkline";
import { type HeroMetric, heroMetrics } from "./dora-metrics";

const KEY_ICON: Record<string, typeof Rocket> = {
  "Deploy frequency": Rocket,
  "Lead time": Timer,
  "Change failure": TriangleAlert,
  "Time to restore": RotateCcw,
};

// TierChip — colored dot + tier label (Elite/High/Medium/Low). Tinted bg from
// the tier color so the verdict reads before the number.
export function TierChip({
  tier,
  label,
  className,
}: {
  tier: Tier;
  label?: string;
  className?: string;
}) {
  const color = TIER_COLOR[tier];
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1.5 rounded-md px-2 py-0.5 font-mono text-[10.5px] font-bold uppercase tracking-wide",
        className,
      )}
      style={{ backgroundColor: TIER_BG[tier], color }}
    >
      <span className="size-1.5 rounded-full" style={{ backgroundColor: color }} />
      {label ?? TIER_LABEL[tier]}
    </span>
  );
}

function DeltaPill({ delta }: { delta: Delta }) {
  if (delta.good === null) {
    return <span className="font-mono text-xs text-muted-foreground">{delta.text}</span>;
  }
  const tone = delta.good ? "text-status-success" : "text-status-failed";
  const Arrow = delta.text.startsWith("−") ? ArrowDown : ArrowUp;
  return (
    <span className={cn("inline-flex items-center gap-0.5 font-mono text-xs font-semibold", tone)}>
      <Arrow className="size-3" aria-hidden />
      {delta.text}
    </span>
  );
}

function MetricCard({ m }: { m: HeroMetric }) {
  const Icon = KEY_ICON[m.key] ?? Rocket;
  return (
    <Card className="gap-0 px-[18px] py-3.5">
      <div className="flex items-start justify-between gap-2">
        <span className="inline-flex items-center gap-1.5 font-mono text-[13px] font-medium uppercase tracking-wide text-muted-foreground">
          <Icon className="size-3.5 text-muted-foreground/70" aria-hidden />
          {m.key}
        </span>
        {m.tier ? (
          <TierChip tier={m.tier} />
        ) : (
          <span className="shrink-0 rounded-md bg-muted px-2 py-0.5 font-mono text-[10.5px] font-bold uppercase tracking-wide text-muted-foreground">
            no data
          </span>
        )}
      </div>
      <div className="mt-2 font-sans text-[30px] font-extrabold leading-none tracking-[-0.04em] text-foreground">
        {m.value}
        {m.unit ? (
          <span className="ml-0.5 text-sm font-semibold text-muted-foreground">{m.unit}</span>
        ) : null}
      </div>
      <div className="mt-1.5 flex items-center gap-2">
        <DeltaPill delta={m.delta} />
        <span className="truncate text-xs text-muted-foreground">{m.vs}</span>
      </div>
      <DoraSparkline values={m.series} color={m.color} className="mt-3.5" />
      <div className="mt-3 flex items-center justify-between border-t border-border pt-2.5 text-[11px]">
        <span className="text-muted-foreground/70">vs. benchmark</span>
        <span className="font-mono text-muted-foreground">{m.bench}</span>
      </div>
    </Card>
  );
}

// DoraHeroCards renders the org rollup: four metric cards (deploy frequency,
// lead time, change failure, time to restore) with tier, delta vs. the prior
// window, sparkline, and benchmark footer.
export function DoraHeroCards({ overview }: { overview: DoraOverview }) {
  const metrics = heroMetrics(overview);
  return (
    <div className="grid gap-3.5 sm:grid-cols-2 xl:grid-cols-4">
      {metrics.map((m) => (
        <MetricCard key={m.key} m={m} />
      ))}
    </div>
  );
}
