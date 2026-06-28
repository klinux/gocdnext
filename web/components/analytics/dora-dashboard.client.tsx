"use client";

import type { Route } from "next";
import { useRouter } from "next/navigation";
import { Activity, GitCommitHorizontal, RotateCcw, Timer } from "lucide-react";

import { cn } from "@/lib/utils";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { DoraGroup } from "@/server/queries/analytics";

const WINDOWS = [
  { value: "7", label: "7 days" },
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
];

// dur formats seconds as a compact human duration (600 → "10m", 90000 → "1d 1h").
function dur(seconds: number): string {
  if (!seconds || seconds <= 0) return "—";
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  if (m > 0) return `${m}m`;
  return `${Math.round(seconds)}s`;
}

function freqLabel(perDay: number): string {
  if (perDay <= 0) return "—";
  if (perDay >= 1) return `${perDay.toFixed(1)}/day`;
  const perWeek = perDay * 7;
  if (perWeek >= 1) return `${perWeek.toFixed(1)}/week`;
  return `${(perDay * 30).toFixed(1)}/month`;
}

function cfrTone(rate: number): string {
  if (rate <= 0.15) return "text-emerald-500";
  if (rate <= 0.3) return "text-amber-500";
  return "text-rose-500";
}

function Metric({
  icon,
  label,
  value,
  tone,
  hint,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  tone?: string;
  hint?: string;
}) {
  return (
    <div className="rounded-lg border border-border/70 bg-muted/20 p-3">
      <div className="flex items-center gap-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
        {icon}
        {label}
      </div>
      <div className={cn("mt-1 text-xl font-semibold tabular-nums", tone)}>
        {value}
      </div>
      {hint ? (
        <div className="text-[11px] text-muted-foreground">{hint}</div>
      ) : null}
    </div>
  );
}

export function DoraDashboard({
  keys,
  activeKey,
  windowDays,
  groups,
}: {
  keys: string[];
  activeKey: string;
  windowDays: number;
  groups: DoraGroup[];
}) {
  const router = useRouter();
  const go = (key: string, win: number) =>
    router.push(
      `/analytics?key=${encodeURIComponent(key)}&window=${win}` as Route,
    );

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center gap-3">
        <div className="flex items-center gap-2">
          <span className="text-sm text-muted-foreground">Group by</span>
          <Select
            items={Object.fromEntries(keys.map((k) => [k, k]))}
            value={activeKey}
            onValueChange={(v) => v && go(v, windowDays)}
          >
            <SelectTrigger aria-label="Group by label key" className="w-40">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {keys.map((k) => (
                <SelectItem key={k} value={k}>
                  {k}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="flex items-center gap-2">
          <span className="text-sm text-muted-foreground">Window</span>
          <Select
            items={Object.fromEntries(WINDOWS.map((w) => [w.value, w.label]))}
            value={String(windowDays)}
            onValueChange={(v) => v && go(activeKey, Number(v))}
          >
            <SelectTrigger aria-label="Time window" className="w-32">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {WINDOWS.map((w) => (
                <SelectItem key={w.value} value={w.value}>
                  {w.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>

      {groups.length === 0 ? (
        <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
          No deployments in this window for any{" "}
          <span className="font-mono">{activeKey}</span> group. DORA metrics
          derive from deploy markers — projects must run a job with a{" "}
          <span className="font-mono">deploy:</span> block.
        </p>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {groups.map((g) => (
            <Card key={g.group}>
              <CardHeader className="pb-2">
                <CardTitle className="flex items-center justify-between text-base">
                  <span className="font-mono">
                    {activeKey}:{g.group}
                  </span>
                  <span className="text-xs font-normal text-muted-foreground tabular-nums">
                    {g.deploys_success}/{g.deploys_total} deploys
                  </span>
                </CardTitle>
              </CardHeader>
              <CardContent className="grid grid-cols-2 gap-2.5">
                <Metric
                  icon={<GitCommitHorizontal className="size-3.5" />}
                  label="Deploy freq"
                  value={freqLabel(g.deploy_freq_per_day)}
                  hint={`${g.deploys_success} in ${windowDays}d`}
                />
                <Metric
                  icon={<Timer className="size-3.5" />}
                  label="Lead time"
                  value={dur(g.lead_time_p50_seconds)}
                  hint="p50 commit→deploy"
                />
                <Metric
                  icon={<Activity className="size-3.5" />}
                  label="Change failure"
                  value={`${Math.round(g.change_failure_rate * 100)}%`}
                  tone={cfrTone(g.change_failure_rate)}
                  hint={`${g.deploys_failed}/${g.deploys_total} failed`}
                />
                <Metric
                  icon={<RotateCcw className="size-3.5" />}
                  label="MTTR"
                  value={dur(g.mttr_p50_seconds)}
                  hint="p50 restore time"
                />
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}
