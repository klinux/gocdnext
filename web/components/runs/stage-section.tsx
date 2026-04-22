import {
  Check,
  ChevronsRight,
  Loader2,
  Minus,
  TriangleAlert,
  X,
} from "lucide-react";

import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { LiveDuration } from "@/components/shared/live-duration";
import { JobCard } from "@/components/runs/job-card";
import type { StageDetail } from "@/types/api";

type Props = {
  stage: StageDetail;
};

// StageSection is the outer chrome for a single stage on the run
// detail page: header row with status glyph + name + duration +
// started-at, a subtle divider, and the list of JobCards below.
// Mirrors the visual language of the projects page's job pills
// (circular tone-tinted badge) in the header so the eye treats
// all "status" cues across the app as one system.
export function StageSection({ stage }: Props) {
  const tone: StatusTone = statusTone(stage.status);
  return (
    <section
      aria-labelledby={`stage-${stage.id}`}
      className="rounded-lg border border-border bg-card"
    >
      <header className="flex flex-wrap items-center gap-3 border-b border-border px-4 py-2.5">
        <span
          className={cn(
            "inline-flex size-5 shrink-0 items-center justify-center rounded-full border-[1.5px]",
            stageGlyphClasses[tone],
            stage.status === "running" && "animate-pulse",
          )}
          aria-hidden
        >
          <StageGlyph tone={tone} />
        </span>
        <span className="text-[10px] text-muted-foreground/70">
          #{stage.ordinal + 1}
        </span>
        <h3
          id={`stage-${stage.id}`}
          className="font-mono text-sm font-semibold uppercase tracking-wider"
        >
          {stage.name}
        </h3>
        <StatusBadge status={stage.status} className="text-[10px]" />
        <div className="ml-auto flex items-center gap-3 text-[11px] text-muted-foreground">
          <LiveDuration
            startedAt={stage.started_at}
            finishedAt={stage.finished_at}
            className="font-mono tabular-nums"
          />
          <span>·</span>
          <span>
            started{" "}
            <RelativeTime at={stage.started_at ?? null} fallback="—" />
          </span>
        </div>
      </header>
      <div className="divide-y divide-border/50">
        {stage.jobs.map((j) => (
          <JobCard key={j.id} job={j} />
        ))}
      </div>
    </section>
  );
}

function StageGlyph({ tone }: { tone: StatusTone }) {
  const cls = "size-2.5";
  switch (tone) {
    case "success":
      return <Check className={cls} aria-hidden strokeWidth={3} />;
    case "failed":
      return <X className={cls} aria-hidden strokeWidth={3} />;
    case "running":
      return <Loader2 className={cn(cls, "animate-spin")} aria-hidden />;
    case "queued":
    case "warning":
      return <TriangleAlert className={cls} aria-hidden />;
    case "canceled":
      return <Minus className={cls} aria-hidden strokeWidth={3} />;
    case "skipped":
    case "neutral":
    default:
      return <ChevronsRight className={cls} aria-hidden strokeWidth={2.5} />;
  }
}

const stageGlyphClasses: Record<StatusTone, string> = {
  success:
    "bg-emerald-500/10 border-emerald-500/40 text-emerald-600 dark:text-emerald-400",
  failed: "bg-red-500/10 border-red-500/40 text-red-600 dark:text-red-400",
  running: "bg-sky-500/10 border-sky-500/40 text-sky-600 dark:text-sky-400",
  queued:
    "bg-amber-500/10 border-amber-500/40 text-amber-700 dark:text-amber-400",
  warning:
    "bg-amber-500/10 border-amber-500/40 text-amber-700 dark:text-amber-400",
  canceled: "bg-muted-foreground/10 border-muted-foreground/40 text-muted-foreground",
  skipped: "bg-muted-foreground/5 border-muted-foreground/30 text-muted-foreground",
  neutral: "bg-muted/40 border-border text-muted-foreground",
};
