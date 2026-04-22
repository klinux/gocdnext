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
import type { JobRunSummaryLite, PipelinePreview } from "@/types/api";

type Props = {
  pipeline: PipelinePreview;
};

// ProjectStagesStrip renders a pipeline as a single tight row of
// circular job pills — jobs flow in stage-order but without
// labels or connectors between stages. Colour on the pill (soft
// bg + tinted border) conveys status; tooltip on hover shows the
// job name + status. Deliberately minimal so the row fits even
// when a card stacks several pipelines.
export function ProjectStagesStrip({ pipeline }: Props) {
  const jobs = flattenJobs(pipeline);
  if (jobs.length === 0) {
    return (
      <p className="text-[10px] italic text-muted-foreground/70">
        No jobs yet.
      </p>
    );
  }
  return (
    <div className="flex flex-wrap items-center gap-1">
      {jobs.map((job, i) => (
        <JobPill
          key={`${job.stage}:${job.name}:${i}`}
          status={job.status}
          label={`${job.stage}:${job.name}`}
        />
      ))}
    </div>
  );
}

type FlatJob = {
  stage: string;
  name: string;
  status: string | undefined;
};

// flattenJobs walks the pipeline's stages in definition order (or
// run order if no definition) and concatenates their jobs into a
// single list. When there are no job_runs attached (never-run or
// in-progress) the stage still contributes a placeholder entry so
// the shape of the pipeline shows up even before the first run.
function flattenJobs(pipeline: PipelinePreview): FlatJob[] {
  const runStages = pipeline.latest_run_stages ?? [];
  const defStages = pipeline.definition_stages ?? [];
  const byName = new Map(runStages.map((s) => [s.name, s]));
  const ordered =
    defStages.length > 0 ? defStages : runStages.map((s) => s.name);

  const out: FlatJob[] = [];
  for (const stageName of ordered) {
    const run = byName.get(stageName);
    const jobs: JobRunSummaryLite[] | undefined = run?.jobs;
    if (jobs && jobs.length > 0) {
      for (const j of jobs) {
        out.push({ stage: stageName, name: j.name, status: j.status });
      }
    } else {
      out.push({ stage: stageName, name: "—", status: undefined });
    }
  }
  return out;
}

function JobPill({
  status,
  label,
}: {
  status: string | undefined;
  label: string;
}) {
  const tone: StatusTone = status ? statusTone(status) : "neutral";
  const tooltip = status ? `${label} · ${status}` : `${label} · not run`;
  return (
    <span
      title={tooltip}
      aria-label={tooltip}
      className={cn(
        "relative inline-flex size-[22px] shrink-0 items-center justify-center rounded-full border-[1.5px]",
        jobPillClasses[tone],
        status === "running" &&
          "after:absolute after:inset-[-3px] after:rounded-full after:border-[1.5px] after:border-sky-500 after:content-[''] after:animate-ping",
      )}
    >
      <JobIcon tone={tone} />
    </span>
  );
}

function JobIcon({ tone }: { tone: StatusTone }) {
  const shared = "size-[10px]";
  switch (tone) {
    case "success":
      return <Check className={shared} aria-hidden strokeWidth={3} />;
    case "failed":
      return <X className={shared} aria-hidden strokeWidth={3} />;
    case "running":
      return <Loader2 className={cn(shared, "animate-spin")} aria-hidden />;
    case "queued":
    case "warning":
      return <TriangleAlert className={shared} aria-hidden />;
    case "canceled":
      return <Minus className={shared} aria-hidden strokeWidth={3} />;
    case "skipped":
    case "neutral":
    default:
      return <ChevronsRight className={shared} aria-hidden strokeWidth={2.5} />;
  }
}

// jobPillClasses — circular 22px badge with soft tinted bg and
// matching border. Success-heavy rows stay quiet; anything not-
// passing (red/blue/amber) pops.
const jobPillClasses: Record<StatusTone, string> = {
  success:
    "bg-emerald-500/10 border-emerald-500/30 text-emerald-600 dark:text-emerald-400",
  failed:
    "bg-red-500/10 border-red-500/30 text-red-600 dark:text-red-400",
  running:
    "bg-sky-500/10 border-sky-500/30 text-sky-600 dark:text-sky-400",
  queued:
    "bg-amber-500/10 border-amber-500/30 text-amber-700 dark:text-amber-400",
  warning:
    "bg-amber-500/10 border-amber-500/30 text-amber-700 dark:text-amber-400",
  canceled:
    "bg-muted-foreground/10 border-muted-foreground/30 text-muted-foreground",
  skipped:
    "bg-muted-foreground/5 border-muted-foreground/20 text-muted-foreground/70",
  neutral:
    "bg-muted/40 border-border text-muted-foreground",
};
