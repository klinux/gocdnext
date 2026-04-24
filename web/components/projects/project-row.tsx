import Link from "next/link";
import type { Route } from "next";
import { Clock, GitBranch } from "lucide-react";

import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import { RelativeTime } from "@/components/shared/relative-time";
import { ProjectStagesStrip } from "@/components/projects/project-stages-strip";
import type { ProjectSummary } from "@/types/api";

type Props = {
  project: ProjectSummary;
};

// ProjectRow is the list-view variant of ProjectCard. Same data,
// laid out horizontally: name + status on the left, the run line +
// stage strip in the middle, a compact commit chip on the right.
// Optimised for density — the user scans a lot of rows at once
// here so each one is slim and the commit footer from the grid
// card collapses to a single trailing chip.
export function ProjectRow({ project }: Props) {
  const primary = project.top_pipelines?.[0];
  const latestStatus = primary?.latest_run_status;
  const meta = project.latest_run_meta;
  const shortSha = meta?.revision ? meta.revision.slice(0, 7) : null;
  const subject = meta?.message ? firstLine(meta.message) : null;

  return (
    <Link
      href={`/projects/${project.slug}` as Route}
      // Explicit column dividers mimic the reference's
      // three-section list row — name | run+stages | commit. The
      // `divide-x` pattern doesn't work here because it targets
      // children of a single flex, and we also need border +
      // rounded corners on the row itself.
      className="group grid grid-cols-[minmax(180px,220px)_1fr_minmax(200px,280px)] items-stretch divide-x divide-border/60 rounded-xl border bg-card shadow-sm transition-colors hover:border-primary/40"
    >
      <div className="flex min-w-0 items-center gap-2 px-4 py-3">
        <div className="min-w-0 flex-1">
          <h3 className="truncate font-mono text-sm font-semibold">
            {project.name}
          </h3>
          {project.description ? (
            <p className="truncate text-[11px] text-muted-foreground">
              {project.description}
            </p>
          ) : null}
        </div>
        <ProjectStatusPill project={project} />
      </div>

      <div className="flex min-w-0 flex-col justify-center gap-2 px-4 py-3">
        {primary ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px]">
            {meta?.branch ? (
              <span
                className="inline-flex items-center gap-1 rounded-md bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground"
                title={`Ref: ${meta.branch}`}
              >
                <GitBranch className="size-3" aria-hidden />
                <span className="max-w-[140px] truncate">{meta.branch}</span>
              </span>
            ) : null}
            {latestStatus ? (
              <RunStatusLine
                status={latestStatus}
                startedAt={primary.latest_run_at}
              />
            ) : null}
          </div>
        ) : null}
        {project.top_pipelines && project.top_pipelines.length > 0 ? (
          <div className="flex flex-wrap items-start gap-x-4 gap-y-2">
            {project.top_pipelines.map((pl) => (
              <div
                key={pl.id}
                className="flex min-w-0 flex-col gap-1"
              >
                <span
                  className="truncate font-mono text-[10px] font-semibold uppercase tracking-wider text-muted-foreground/80"
                  title={pl.name}
                >
                  {pl.name}
                </span>
                <ProjectStagesStrip pipeline={pl} />
              </div>
            ))}
          </div>
        ) : null}
      </div>

      <div className="flex min-w-0 flex-col items-end justify-center gap-1 px-4 py-3 text-[11px] text-muted-foreground">
        {shortSha || subject ? (
          <div className="flex min-w-0 items-center gap-1.5">
            {shortSha ? (
              <span className="font-mono text-foreground">{shortSha}</span>
            ) : null}
            {subject ? (
              <span className="max-w-[200px] truncate" title={meta?.message}>
                {subject}
              </span>
            ) : null}
          </div>
        ) : null}
        <div className="flex items-center gap-2 text-[10px]">
          {meta?.author || meta?.triggered_by ? (
            <AuthorChip
              name={(meta.author || meta.triggered_by) as string}
              kind={meta.author ? "commit" : "trigger"}
            />
          ) : null}
          {project.latest_run_at ? (
            <span className="inline-flex items-center gap-1">
              <Clock className="size-3" aria-hidden />
              <RelativeTime at={project.latest_run_at} />
            </span>
          ) : null}
        </div>
      </div>
    </Link>
  );
}

function ProjectStatusPill({ project }: { project: ProjectSummary }) {
  const tone: StatusTone =
    project.status === "running"
      ? "running"
      : project.status === "success"
        ? "success"
        : project.status === "failing"
          ? "failed"
          : "neutral";
  const label =
    project.status === "no_pipelines"
      ? "empty"
      : project.status === "never_run"
        ? "new"
        : project.status;
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-full border px-2 py-0.5 text-[10px] font-medium",
        statusPillToneClasses[tone],
      )}
    >
      <span
        className={cn(
          "inline-block size-1.5 rounded-full",
          toneBgClasses[tone],
          tone === "running" && "animate-pulse",
        )}
        aria-hidden
      />
      {label}
    </span>
  );
}

function RunStatusLine({
  status,
  startedAt,
}: {
  status: string;
  startedAt?: string;
}) {
  const tone = statusTone(status);
  return (
    <span className="ml-auto inline-flex items-center gap-1.5 text-muted-foreground">
      <span
        className={cn(
          "inline-block size-1.5 rounded-full",
          toneBgClasses[tone],
          status === "running" && "animate-pulse",
        )}
        aria-hidden
      />
      <span className="font-medium">{status}</span>
      {startedAt ? (
        <>
          <span>·</span>
          <RelativeTime at={startedAt} />
        </>
      ) : null}
    </span>
  );
}

function AuthorChip({
  name,
  kind,
}: {
  name: string;
  kind?: "commit" | "trigger";
}) {
  const initials = initialsFrom(name);
  const title =
    kind === "trigger" ? `Triggered by ${name}` : `Authored by ${name}`;
  return (
    <span className="inline-flex shrink-0 items-center gap-1">
      <span
        className="inline-flex size-4 items-center justify-center rounded-full bg-muted font-mono text-[8px] font-semibold text-muted-foreground"
        aria-hidden
      >
        {initials}
      </span>
      <span className="max-w-[80px] truncate" title={title}>
        {name}
      </span>
    </span>
  );
}

function initialsFrom(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0]!.slice(0, 2).toUpperCase();
  return (parts[0]![0]! + parts[parts.length - 1]![0]!).toUpperCase();
}

function firstLine(msg: string): string {
  const idx = msg.indexOf("\n");
  return idx >= 0 ? msg.slice(0, idx) : msg;
}

const statusPillToneClasses: Record<StatusTone, string> = {
  success:
    "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
  failed: "border-red-500/30 bg-red-500/10 text-red-700 dark:text-red-400",
  running: "border-sky-500/30 bg-sky-500/10 text-sky-700 dark:text-sky-400",
  queued:
    "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  warning:
    "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  awaiting:
    "border-amber-500/50 bg-amber-500/15 text-amber-700 dark:text-amber-400",
  canceled: "border-muted-foreground/30 bg-muted text-muted-foreground",
  skipped: "border-muted-foreground/20 bg-muted/50 text-muted-foreground",
  neutral: "border-border bg-muted/40 text-muted-foreground",
};

const toneBgClasses: Record<StatusTone, string> = {
  success: "bg-emerald-500",
  failed: "bg-red-500",
  running: "bg-sky-500",
  queued: "bg-amber-500",
  warning: "bg-amber-500",
  awaiting: "bg-amber-500",
  canceled: "bg-muted-foreground",
  skipped: "bg-muted-foreground/60",
  neutral: "bg-muted-foreground/40",
};
