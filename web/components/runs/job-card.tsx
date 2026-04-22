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
import { RelativeTime } from "@/components/shared/relative-time";
import { LiveDuration } from "@/components/shared/live-duration";
import { LogViewer } from "@/components/runs/log-viewer";
import type { JobDetail } from "@/types/api";

type Props = {
  job: JobDetail;
};

// JobCard renders one job inside a stage's section. Visually a row
// (not a card) so the outer stage-section container owns the
// border/radius. Circular tone-tinted glyph on the left mirrors the
// projects page's job pills — same system, different context.
export function JobCard({ job }: Props) {
  const tone: StatusTone = statusTone(job.status);
  const hasLogs = (job.logs?.length ?? 0) > 0;

  return (
    <div
      id={`job-${job.id}`}
      // :target highlights the row when the URL hash matches — the
      // project-page "View logs" deep-links here and the ring lets
      // the user spot the row after the scroll.
      className={cn(
        "scroll-mt-20 px-4 py-3 transition-colors",
        "[&:target]:bg-primary/5 [&:target]:ring-1 [&:target]:ring-primary/40",
      )}
    >
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span
          className={cn(
            "inline-flex size-5 shrink-0 items-center justify-center rounded-full border-[1.5px]",
            jobGlyphClasses[tone],
            job.status === "running" && "animate-pulse",
          )}
          aria-hidden
          title={job.status}
        >
          <JobGlyph tone={tone} />
        </span>
        <span className="font-mono text-sm font-semibold">{job.name}</span>
        {job.matrix_key ? (
          <span className="font-mono text-[11px] text-muted-foreground">
            [{job.matrix_key}]
          </span>
        ) : null}
        <div className="ml-auto flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
          {job.image ? (
            <Meta label="image" value={job.image} truncate />
          ) : null}
          {typeof job.exit_code === "number" ? (
            <Meta
              label="exit"
              value={String(job.exit_code)}
              className={
                job.exit_code !== 0
                  ? "text-red-500 font-semibold"
                  : undefined
              }
            />
          ) : null}
          <Meta
            label="started"
            value={<RelativeTime at={job.started_at ?? null} fallback="—" />}
          />
          <LiveDuration
            startedAt={job.started_at}
            finishedAt={job.finished_at}
            className="font-mono tabular-nums text-foreground"
          />
        </div>
      </div>

      {job.error ? (
        <p className="mt-2 rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-xs text-red-600 dark:text-red-400">
          {job.error}
        </p>
      ) : null}

      <details open={hasLogs} className="mt-2">
        <summary className="cursor-pointer select-none text-[11px] text-muted-foreground hover:text-foreground">
          Logs ({job.logs?.length ?? 0})
        </summary>
        <div className="mt-2 overflow-hidden rounded-md border border-border">
          <LogViewer logs={job.logs ?? []} />
        </div>
      </details>
    </div>
  );
}

function Meta({
  label,
  value,
  className,
  truncate,
}: {
  label: string;
  value: React.ReactNode;
  className?: string;
  truncate?: boolean;
}) {
  return (
    <span className={cn("inline-flex items-center gap-1", className)}>
      <span className="text-[9px] font-semibold uppercase tracking-wider text-muted-foreground/70">
        {label}
      </span>
      <span
        className={cn(
          "font-mono",
          truncate && "max-w-[200px] truncate",
        )}
      >
        {value}
      </span>
    </span>
  );
}

function JobGlyph({ tone }: { tone: StatusTone }) {
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

const jobGlyphClasses: Record<StatusTone, string> = {
  success:
    "bg-emerald-500/10 border-emerald-500/40 text-emerald-600 dark:text-emerald-400",
  failed: "bg-red-500/10 border-red-500/40 text-red-600 dark:text-red-400",
  running: "bg-sky-500/10 border-sky-500/40 text-sky-600 dark:text-sky-400",
  queued:
    "bg-amber-500/10 border-amber-500/40 text-amber-700 dark:text-amber-400",
  warning:
    "bg-amber-500/10 border-amber-500/40 text-amber-700 dark:text-amber-400",
  canceled:
    "bg-muted-foreground/10 border-muted-foreground/40 text-muted-foreground",
  skipped:
    "bg-muted-foreground/5 border-muted-foreground/30 text-muted-foreground",
  neutral: "bg-muted/40 border-border text-muted-foreground",
};
