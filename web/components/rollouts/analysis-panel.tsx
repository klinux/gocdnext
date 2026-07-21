import { LineChart } from "lucide-react";

import { cn } from "@/lib/utils";
import { analysisTone, TONE } from "@/lib/rollouts";
import type { RolloutAnalysis } from "@/types/api";

type Props = { analysis: RolloutAnalysis | null };

// AnalysisPanel surfaces the active AnalysisRun that gates a canary. Per-metric
// detail is NOT in the read API yet, so this shows the run name + phase verdict
// + (bounded) message only — the summary the API carries.
export function AnalysisPanel({ analysis }: Props) {
  if (!analysis) {
    return (
      <div className="rounded-xl border border-border bg-muted/20 p-4 text-sm text-muted-foreground">
        No metric analysis is running for this rollout.
      </div>
    );
  }

  const tone = analysisTone(analysis.phase);
  return (
    <div className="rounded-xl border border-border bg-muted/20 p-4">
      <div className="flex items-center justify-between gap-3">
        <span className="flex items-center gap-2 font-mono text-xs font-medium text-foreground">
          <LineChart className="size-4 text-teal-500" aria-hidden />
          {analysis.name || "analysis"}
        </span>
        <span
          className={cn(
            "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-semibold",
            TONE[tone],
          )}
        >
          {analysis.phase || "Unknown"}
        </span>
      </div>

      {analysis.message ? (
        <p className="mt-3 border-t border-border/60 pt-3 text-xs text-muted-foreground">
          {analysis.message}
        </p>
      ) : null}

      <p className="mt-3 text-[11px] text-muted-foreground/80">
        Per-metric detail is not exposed by the read API yet — showing the run
        summary.
      </p>
    </div>
  );
}
