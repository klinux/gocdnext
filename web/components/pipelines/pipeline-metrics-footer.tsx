import { Activity, AlertTriangle, CheckCircle2, Clock } from "lucide-react";

import { cn } from "@/lib/utils";
import { formatDurationSeconds } from "@/lib/format";
import type { PipelineMetrics } from "@/types/api";
import type { Bottleneck } from "@/components/pipelines/pipeline-card-helpers";

// PipelineMetricsFooter is the DORA-flavoured strip at the bottom
// of each card: lead p50, process p50, success rate, run count +
// window. Deliberately a single row so it doesn't dominate the
// card — aggregates are context, not the main read.
export function PipelineMetricsFooter({
  metrics,
}: {
  metrics: PipelineMetrics;
}) {
  if (metrics.runs_considered === 0) {
    // Footer is useless when we have no terminal runs. The parent
    // renders an explanatory empty state instead.
    return null;
  }
  const rate = Math.round(metrics.success_rate * 100);
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-border bg-muted/30 px-3 py-2 text-[10px] text-muted-foreground">
      <MetricCell
        icon={<Clock className="size-3" />}
        label="Lead p50"
        value={formatDurationSeconds(metrics.lead_time_p50_seconds)}
      />
      <MetricCell
        icon={<Activity className="size-3" />}
        label="Process p50"
        value={formatDurationSeconds(metrics.process_time_p50_seconds)}
      />
      <MetricCell
        icon={<CheckCircle2 className="size-3" />}
        label="Success"
        value={`${rate}%`}
        valueClass={cn(
          rate >= 90
            ? "text-emerald-500"
            : rate >= 70
              ? "text-amber-500"
              : "text-red-500",
        )}
      />
      <span className="ml-auto text-[10px] tabular-nums">
        {metrics.runs_considered} runs · {metrics.window_days}d
      </span>
    </div>
  );
}

function MetricCell({
  icon,
  label,
  value,
  valueClass,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  valueClass?: string;
}) {
  return (
    <span className="inline-flex items-center gap-1">
      {icon}
      <span className="uppercase tracking-wide">{label}</span>
      <span className={cn("font-mono text-foreground tabular-nums", valueClass)}>
        {value}
      </span>
    </span>
  );
}

export function PipelineBottleneckCallout({
  bottleneck,
}: {
  bottleneck: Bottleneck;
}) {
  const parts: string[] = [];
  if (bottleneck.overP50Sec != null) {
    parts.push(`+${formatDurationSeconds(bottleneck.overP50Sec)} over p50`);
  }
  if (bottleneck.successRate != null) {
    parts.push(`${Math.round(bottleneck.successRate * 100)}% C/A — flaky`);
  }
  return (
    <div className="flex items-start gap-2 border-t border-amber-500/30 bg-amber-500/5 px-3 py-1.5 text-[11px] text-amber-700 dark:text-amber-400">
      <AlertTriangle className="mt-0.5 size-3 shrink-0" aria-hidden />
      <span>
        <span className="font-mono font-semibold">{bottleneck.stageName}</span>{" "}
        is the bottleneck: {parts.join(" · ")}
      </span>
    </div>
  );
}
