import {
  Bell,
  Check,
  ChevronsRight,
  Gavel,
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
import { ApprovalButtons } from "@/components/runs/approval-buttons.client";
import type { JobDetail } from "@/types/api";

// Server-side mirror: `_notify_<idx>`. Kept as a prefix rather
// than importing a constant so it survives minor renames on the
// Go side as long as the prefix holds; UI degrades to the slug
// if the shape changes.
const SYNTH_NOTIFY_PREFIX = "_notify_";

type Props = {
  job: JobDetail;
  // runID enables the ApprovalButtons revalidation path — the
  // server action revalidates `/runs/[runID]` after a decision.
  runID: string;
};

// JobCard renders one job inside a stage's section. Visually a row
// (not a card) so the outer stage-section container owns the
// border/radius. Circular tone-tinted glyph on the left mirrors the
// projects page's job pills — same system, different context.
// Synth notification jobs (`_notify_<idx>`) show the plugin ref as
// their label + a trigger pill ("on failure" / "on success" / …)
// so the user never sees the raw index-encoded slug.
export function JobCard({ job, runID }: Props) {
  const tone: StatusTone = statusTone(job.status);
  const hasLogs = (job.logs?.length ?? 0) > 0;
  const awaiting = job.status === "awaiting_approval" && job.approval_gate;
  const decided = job.approval_gate && !!job.decision;
  const isNotify = job.name.startsWith(SYNTH_NOTIFY_PREFIX);
  const displayName = isNotify ? job.notify_uses || job.name : job.name;

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
          {isNotify ? (
            <Bell className="size-2.5" aria-hidden strokeWidth={2.5} />
          ) : (
            <JobGlyph tone={tone} />
          )}
        </span>
        <span className="font-mono text-sm font-semibold">{displayName}</span>
        {isNotify && job.notify_on ? (
          <span
            className={cn(
              "rounded-md border px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wider",
              notifyTriggerClasses[job.notify_on] ??
                "border-border bg-muted/50 text-muted-foreground",
            )}
          >
            on {job.notify_on}
          </span>
        ) : null}
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

      {awaiting ? (
        <div className="mt-3 rounded-md border border-amber-500/40 bg-amber-500/5 px-3 py-2 text-sm">
          <div className="flex items-center gap-2">
            <Gavel className="h-4 w-4 text-amber-600 dark:text-amber-400" aria-hidden />
            <span className="font-medium text-amber-700 dark:text-amber-300">
              Awaiting approval
            </span>
            {job.awaiting_since ? (
              <span className="text-xs text-muted-foreground">
                · waiting since{" "}
                <RelativeTime at={job.awaiting_since} fallback="—" />
              </span>
            ) : null}
          </div>
          {job.approval_description ? (
            <p className="mt-1 text-xs text-muted-foreground">
              {job.approval_description}
            </p>
          ) : null}
          <ApprovalButtons
            jobRunID={job.id}
            runID={runID}
            jobName={job.name}
            description={job.approval_description}
            approvers={job.approvers}
          />
        </div>
      ) : null}

      {decided ? (
        <p className="mt-2 text-xs text-muted-foreground">
          {job.decision === "approved" ? "Approved" : "Rejected"}
          {job.decided_by ? ` by ${job.decided_by}` : ""}
          {job.decided_at ? (
            <>
              {" · "}
              <RelativeTime at={job.decided_at} fallback="—" />
            </>
          ) : null}
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

// Trigger pill colours. Loosely tracks the tone each outcome
// implies: red-ish for failure, emerald for success, amber for
// canceled (same as "queued/warning" in the status palette),
// muted for always since it's the unconditional case.
const notifyTriggerClasses: Record<string, string> = {
  failure:
    "border-red-500/40 bg-red-500/10 text-red-700 dark:text-red-400",
  success:
    "border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
  canceled:
    "border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  always:
    "border-border bg-muted/50 text-muted-foreground",
};

const jobGlyphClasses: Record<StatusTone, string> = {
  success:
    "bg-emerald-500/10 border-emerald-500/40 text-emerald-600 dark:text-emerald-400",
  failed: "bg-red-500/10 border-red-500/40 text-red-600 dark:text-red-400",
  running: "bg-sky-500/10 border-sky-500/40 text-sky-600 dark:text-sky-400",
  queued:
    "bg-amber-500/10 border-amber-500/40 text-amber-700 dark:text-amber-400",
  warning:
    "bg-amber-500/10 border-amber-500/40 text-amber-700 dark:text-amber-400",
  awaiting:
    "bg-amber-500/15 border-amber-500/60 text-amber-700 dark:text-amber-400",
  canceled:
    "bg-muted-foreground/10 border-muted-foreground/40 text-muted-foreground",
  skipped:
    "bg-muted-foreground/5 border-muted-foreground/30 text-muted-foreground",
  neutral: "bg-muted/40 border-border text-muted-foreground",
};
