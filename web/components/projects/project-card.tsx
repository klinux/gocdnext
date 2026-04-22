import Link from "next/link";
import type { Route } from "next";
import { Clock, GitBranch } from "lucide-react";

import { cn } from "@/lib/utils";
import { statusTone, type StatusTone } from "@/lib/status";
import { formatDurationSeconds } from "@/lib/format";
import { RelativeTime } from "@/components/shared/relative-time";
import { ProjectStagesStrip } from "@/components/projects/project-stages-strip";
import type { ProjectSummary } from "@/types/api";

type Props = {
  project: ProjectSummary;
};

// ProjectCard is the redesigned grid tile — structured like the
// reference mockup: header with name, short description, a latest
// run line (branch + counter + status + duration), the stage grid
// with job dots, a KPI strip (success/avg duration/runs over the
// window), and a commit footer (sha + subject + author + when).
// Status stays implicit via the run line + stage colours — no
// duplicate badge in the header.
export function ProjectCard({ project }: Props) {
  const pipelines = project.top_pipelines ?? [];
  const primary = pipelines[0];
  const latestStatus = primary?.latest_run_status;
  const meta = project.latest_run_meta;
  const metrics = project.metrics;

  return (
    <Link
      href={`/projects/${project.slug}` as Route}
      className="group flex flex-col overflow-hidden rounded-xl border bg-card shadow-sm transition-colors hover:border-primary/40"
    >
      {/* flex-1 on the content wrapper pushes the footer to the
          bottom when the grid row stretches this card to match a
          taller neighbour — without it the footer floats up and
          leaves empty space below on uneven cards. */}
      <div className="flex flex-1 flex-col gap-3 p-4">
        <div className="flex items-start gap-2">
          <div className="min-w-0 flex-1">
            <h3 className="truncate font-mono text-sm font-semibold">
              {project.name}
            </h3>
            {project.description ? (
              <p className="mt-0.5 line-clamp-1 text-xs text-muted-foreground">
                {project.description}
              </p>
            ) : null}
          </div>
          <ProjectStatusPill project={project} />
        </div>

        {primary && (meta?.branch || latestStatus || primary.latest_run_at) ? (
          <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 text-[11px]">
            {meta?.branch ? (
              <span
                className="inline-flex items-center gap-1 rounded-md bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground"
                title={`Ref: ${meta.branch}`}
              >
                <GitBranch className="size-3" aria-hidden />
                <span className="max-w-[160px] truncate">{meta.branch}</span>
              </span>
            ) : null}
            {latestStatus ? (
              <RunStatusLine
                status={latestStatus}
                startedAt={primary.latest_run_at}
              />
            ) : (
              <span className="italic text-muted-foreground">Never run</span>
            )}
          </div>
        ) : null}

        {pipelines.length > 0 ? (
          <div className="flex flex-wrap items-start gap-x-4 gap-y-3">
            {pipelines.map((pl) => (
              <div
                key={pl.id}
                className="flex min-w-0 flex-col gap-1.5"
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
            {/* Backend caps `top_pipelines` at 20. For projects
                past that, surface a trailing hint so the user
                knows the card is a preview, not the complete
                list — click-through to the detail page shows
                everything. */}
            {project.pipeline_count > pipelines.length ? (
              <span
                className="self-center font-mono text-[10px] text-muted-foreground/70"
                title="Click the card to see all pipelines"
              >
                +{project.pipeline_count - pipelines.length} more
              </span>
            ) : null}
          </div>
        ) : null}

        {metrics && metrics.runs_considered > 0 ? (
          <KpiStrip
            windowDays={metrics.window_days}
            successRate={metrics.success_rate}
            avgDurationSec={metrics.process_time_p50_seconds}
            runs={metrics.runs_considered}
          />
        ) : null}
      </div>

      {meta?.revision || meta?.author || meta?.triggered_by ? (
        <CommitFooter
          sha={meta.revision}
          message={meta.message}
          author={meta.author || meta.triggered_by}
          authorKind={meta.author ? "commit" : "trigger"}
          at={project.latest_run_at}
        />
      ) : null}
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

function KpiStrip({
  windowDays,
  successRate,
  avgDurationSec,
  runs,
}: {
  windowDays: number;
  successRate: number;
  avgDurationSec: number;
  runs: number;
}) {
  const ratePct = Math.round(successRate * 100);
  return (
    <div className="mt-1 grid grid-cols-3 gap-3 border-t border-dashed border-border pt-3">
      <KpiWithBar
        label={`Success · ${windowDays}D`}
        value={`${ratePct}%`}
        percent={ratePct}
      />
      <Kpi label="Avg duration" value={formatDurationSeconds(avgDurationSec)} />
      <Kpi label={`Runs · ${windowDays}D`} value={runs.toLocaleString()} />
    </div>
  );
}

function Kpi({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <span className="truncate text-[9px] font-semibold uppercase tracking-wider text-muted-foreground">
        {label}
      </span>
      <span className="truncate font-mono text-sm font-semibold tabular-nums leading-none text-foreground">
        {value}
      </span>
    </div>
  );
}

// KpiWithBar adds a 3px progress bar below the value, following the
// design's `mini-metric .bar` pattern — tints emerald/amber/red by
// threshold so a quick glance tells you if this project is above
// or below target.
function KpiWithBar({
  label,
  value,
  percent,
}: {
  label: string;
  value: string;
  percent: number;
}) {
  const barClass =
    percent >= 95
      ? "bg-emerald-500"
      : percent >= 90
        ? "bg-amber-500"
        : "bg-red-500";
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <span className="truncate text-[9px] font-semibold uppercase tracking-wider text-muted-foreground">
        {label}
      </span>
      <span className="truncate font-mono text-sm font-semibold tabular-nums leading-none text-foreground">
        {value}
      </span>
      <div className="mt-0.5 h-[3px] w-full overflow-hidden rounded-sm bg-muted">
        <span
          className={cn("block h-full", barClass)}
          style={{ width: `${Math.max(0, Math.min(100, percent))}%` }}
        />
      </div>
    </div>
  );
}

function CommitFooter({
  sha,
  message,
  author,
  authorKind,
  at,
}: {
  sha?: string;
  message?: string;
  author?: string;
  authorKind?: "commit" | "trigger";
  at?: string;
}) {
  const subject = message ? firstLine(message) : null;
  const shortSha = sha ? sha.slice(0, 7) : null;
  return (
    <div className="flex items-center gap-2 border-t border-border bg-muted/30 px-4 py-2 text-[11px] text-muted-foreground">
      <div className="flex min-w-0 flex-1 items-center gap-2">
        {shortSha ? (
          <span className="shrink-0 rounded-sm bg-muted/80 px-1.5 py-0.5 font-mono text-[10px] text-foreground/80">
            {shortSha}
          </span>
        ) : null}
        {subject ? (
          <span
            className="truncate text-muted-foreground"
            title={message}
          >
            {subject}
          </span>
        ) : null}
      </div>
      {author ? <AuthorChip name={author} kind={authorKind} /> : null}
      {at ? (
        <span className="inline-flex items-center gap-1 text-[10px]">
          <Clock className="size-3" aria-hidden />
          <RelativeTime at={at} />
        </span>
      ) : null}
    </div>
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
    <span className="inline-flex shrink-0 items-center gap-1 text-[10px]">
      <span
        // Gradient avatar mini from the design — warm tones that
        // don't compete with status colours elsewhere on the card.
        className="inline-flex size-[18px] items-center justify-center rounded-full bg-gradient-to-br from-amber-300/80 to-amber-700/80 font-mono text-[9px] font-semibold text-white shadow-sm"
        aria-hidden
      >
        {initials}
      </span>
      <span className="max-w-[100px] truncate" title={title}>
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
  canceled: "bg-muted-foreground",
  skipped: "bg-muted-foreground/60",
  neutral: "bg-muted-foreground/40",
};
