"use client";

import {
  Ban,
  CheckCircle2,
  ChevronRight,
  Clock,
  CircleDashed,
  Loader2,
  MinusCircle,
  XCircle,
} from "lucide-react";
import type { ComponentType } from "react";

import { cn } from "@/lib/utils";
import { durationBetween, formatDurationSeconds } from "@/lib/format";
import type { JobDetail, StageDetail } from "@/types/api";

type Props = {
  stages: StageDetail[];
};

// PipelineCanvas is the "pipeline view" at the top of a run's
// detail page: stages as columns left-to-right, jobs as pills
// inside, chevron connectors between. Clicking a job scrolls the
// matching JobCard (rendered below via StageSection) into view —
// avoids a second log viewer that would lag the primary one.

export function PipelineCanvas({ stages }: Props) {
  if (stages.length === 0) {
    return null;
  }
  return (
    <section aria-label="Pipeline" className="-mx-2 overflow-x-auto px-2 pb-2">
      <ol className="flex min-w-full items-stretch gap-2">
        {stages.map((stage, i) => (
          <li key={stage.id} className="flex items-stretch">
            <StageColumn stage={stage} />
            {i < stages.length - 1 ? (
              <Connector
                previousStatus={stage.status}
                nextStatus={stages[i + 1]!.status}
              />
            ) : null}
          </li>
        ))}
      </ol>
    </section>
  );
}

// --- column ---

function StageColumn({ stage }: { stage: StageDetail }) {
  const tone = statusTone(stage.status);
  const duration = formatDurationSeconds(
    durationBetween(stage.started_at, stage.finished_at),
  );
  return (
    <div
      className={cn(
        "flex min-w-[220px] max-w-[260px] flex-col rounded-lg border bg-card",
        tone.border,
      )}
      data-status={stage.status}
    >
      <header
        className={cn(
          "flex items-center gap-2 border-b px-3 py-2 text-xs font-medium",
          tone.header,
        )}
      >
        <StatusGlyph status={stage.status} className="size-3.5" />
        <span className="truncate">
          <span className="text-[10px] text-muted-foreground/80 mr-1">
            #{stage.ordinal + 1}
          </span>
          {stage.name}
        </span>
        <span className="ml-auto font-mono text-[10px] text-muted-foreground">
          {duration}
        </span>
      </header>
      <div className="flex flex-col gap-1.5 p-2">
        {stage.jobs.length === 0 ? (
          <p className="rounded-md border border-dashed px-2 py-1.5 text-center text-[11px] text-muted-foreground">
            no jobs
          </p>
        ) : (
          stage.jobs.map((j) => <JobPill key={j.id} job={j} />)
        )}
      </div>
    </div>
  );
}

// --- job pill ---

function JobPill({ job }: { job: JobDetail }) {
  const tone = statusTone(job.status);
  const duration = formatDurationSeconds(
    durationBetween(job.started_at, job.finished_at),
  );
  const label = job.matrix_key
    ? `${job.name} [${job.matrix_key}]`
    : job.name;

  const scrollToJob = () => {
    // The JobCard rendered below in StageSection carries id
    // job-<uuid>. We add that id in job-card.tsx.
    if (typeof document === "undefined") return;
    const target = document.getElementById(`job-${job.id}`);
    if (target) {
      target.scrollIntoView({ behavior: "smooth", block: "center" });
      target.classList.add("ring-2", "ring-primary/40");
      setTimeout(() => target.classList.remove("ring-2", "ring-primary/40"), 1200);
    }
  };

  return (
    <button
      type="button"
      onClick={scrollToJob}
      className={cn(
        "group flex items-center gap-1.5 rounded-md border px-2 py-1 text-left text-xs transition-colors hover:bg-muted/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40",
        tone.pillBorder,
        tone.pillBg,
      )}
      title={job.error || undefined}
    >
      <StatusGlyph status={job.status} className={cn("size-3.5", tone.glyph)} />
      <span className={cn("flex-1 truncate font-mono", tone.text)}>{label}</span>
      <span className="font-mono text-[10px] text-muted-foreground/80">
        {duration}
      </span>
    </button>
  );
}

// --- connector between columns ---

function Connector({
  previousStatus,
  nextStatus,
}: {
  previousStatus: string;
  nextStatus: string;
}) {
  // Connector color follows the downstream state: if the next
  // stage is running we want the flow-line to feel "active". If
  // previous failed + downstream is canceled, we dim the line to
  // communicate the blocked edge.
  const tone = statusTone(isDim(previousStatus) ? previousStatus : nextStatus);
  return (
    <span
      aria-hidden
      className={cn(
        "mx-1 flex shrink-0 items-center",
        previousStatus === "running" ? "text-primary" : tone.glyph,
      )}
    >
      <ChevronRight className="size-4" />
    </span>
  );
}

function isDim(status: string): boolean {
  return status === "canceled" || status === "skipped" || status === "failed";
}

// --- status glyph ---

function StatusGlyph({ status, className }: { status: string; className?: string }) {
  const Icon = iconFor(status);
  const cls = cn(
    className,
    status === "running" && "animate-spin",
  );
  return <Icon className={cls} />;
}

function iconFor(status: string): ComponentType<{ className?: string }> {
  switch (status) {
    case "success":
      return CheckCircle2;
    case "failed":
      return XCircle;
    case "running":
      return Loader2;
    case "queued":
      return Clock;
    case "canceled":
      return Ban;
    case "skipped":
      return MinusCircle;
    case "waiting":
    default:
      return CircleDashed;
  }
}

// --- tone palette ---
//
// Central place to tweak the whole pipeline-canvas look. Each status
// returns the class set used by the stage column, the job pill + the
// connector glyph. Keeping it in one table makes it cheap to slot
// into the design-system slice later: point these at tokens
// (`bg-status-success`, …) instead of raw Tailwind colors.

type Tone = {
  border: string;
  header: string;
  glyph: string;
  pillBg: string;
  pillBorder: string;
  text: string;
};

const TONE: Record<string, Tone> = {
  success: {
    border: "border-emerald-500/30",
    header: "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
    glyph: "text-emerald-600 dark:text-emerald-400",
    pillBg: "bg-emerald-500/5",
    pillBorder: "border-emerald-500/20",
    text: "text-emerald-900 dark:text-emerald-100",
  },
  failed: {
    border: "border-rose-500/40",
    header: "bg-rose-500/10 text-rose-700 dark:text-rose-300",
    glyph: "text-rose-600 dark:text-rose-400",
    pillBg: "bg-rose-500/5",
    pillBorder: "border-rose-500/30",
    text: "text-rose-900 dark:text-rose-100",
  },
  running: {
    border: "border-primary/40",
    header: "bg-primary/10 text-primary",
    glyph: "text-primary",
    pillBg: "bg-primary/5",
    pillBorder: "border-primary/30",
    text: "text-foreground",
  },
  queued: {
    border: "border-border",
    header: "bg-muted/60 text-muted-foreground",
    glyph: "text-muted-foreground",
    pillBg: "bg-background",
    pillBorder: "border-border",
    text: "text-foreground",
  },
  canceled: {
    border: "border-border/70 border-dashed",
    header: "bg-muted/30 text-muted-foreground",
    glyph: "text-muted-foreground",
    pillBg: "bg-muted/20",
    pillBorder: "border-border/60 border-dashed",
    text: "text-muted-foreground",
  },
  skipped: {
    border: "border-border/70 border-dashed",
    header: "bg-muted/30 text-muted-foreground",
    glyph: "text-muted-foreground",
    pillBg: "bg-muted/20",
    pillBorder: "border-border/60 border-dashed",
    text: "text-muted-foreground",
  },
  waiting: {
    border: "border-border",
    header: "bg-background text-muted-foreground",
    glyph: "text-muted-foreground",
    pillBg: "bg-background",
    pillBorder: "border-border",
    text: "text-muted-foreground",
  },
};

function statusTone(status: string): Tone {
  return TONE[status] ?? TONE.waiting!;
}
