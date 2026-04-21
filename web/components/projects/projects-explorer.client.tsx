"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import {
  ArrowRight,
  Check,
  CheckCircle2,
  ChevronsRight,
  CircleDashed,
  GitBranch,
  Loader2,
  Minus,
  Search,
  TriangleAlert,
  X,
  XCircle,
} from "lucide-react";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { RelativeTime } from "@/components/shared/relative-time";
import { statusTone, type StatusTone } from "@/lib/status";
import type {
  PipelinePreview,
  ProjectProvider,
  ProjectStatus,
  ProjectSummary,
  StageRunSummary,
} from "@/types/api";

type Props = { projects: ProjectSummary[] };

// ProjectsExplorer owns the Fuselet-style list: search + filter
// pills + card grid. Stays client-side because search and filter
// state should feel instant; the dataset is small enough (dozens
// of projects) that filtering in memory is cheaper than a
// round-trip. If it ever outgrows that, move to Server Actions
// with query params.
export function ProjectsExplorer({ projects }: Props) {
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState<ProjectStatus | "all">("all");
  const [provider, setProvider] = useState<ProjectProvider | "all">("all");

  const statusCounts = useMemo(() => countBy(projects, (p) => p.status), [projects]);
  const providerCounts = useMemo(
    () => countBy(projects, (p) => p.provider ?? ""),
    [projects],
  );

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return projects.filter((p) => {
      if (status !== "all" && p.status !== status) return false;
      if (provider !== "all" && (p.provider ?? "") !== provider) return false;
      if (!q) return true;
      return (
        p.slug.toLowerCase().includes(q) ||
        p.name.toLowerCase().includes(q) ||
        (p.description ?? "").toLowerCase().includes(q)
      );
    });
  }, [projects, query, status, provider]);

  return (
    <div className="space-y-5">
      <div className="relative">
        <Search
          className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
          aria-hidden
        />
        <Input
          value={query}
          // base-ui's Input primitive emits change via onValueChange
          // (see @base-ui/react/input.d.ts). Using the native onChange
          // against the controlled `value` leaves the component stuck
          // on the initial empty string — search would render but never
          // filter.
          onValueChange={(next: string) => setQuery(next)}
          placeholder="Search by slug, name or description..."
          className="h-11 pl-9 text-sm"
          aria-label="Search projects"
        />
      </div>

      <div className="flex flex-wrap items-center gap-2 text-xs">
        <FilterPill
          label="All"
          count={projects.length}
          active={status === "all"}
          onClick={() => setStatus("all")}
          tone="all"
        />
        {(
          ["success", "running", "failing", "never_run", "no_pipelines"] as const
        ).map((s) =>
          (statusCounts[s] ?? 0) > 0 ? (
            <FilterPill
              key={s}
              label={statusLabel(s)}
              count={statusCounts[s] ?? 0}
              active={status === s}
              onClick={() => setStatus(s)}
              tone={statusToTone(s)}
            />
          ) : null,
        )}

        {(["github", "gitlab", "bitbucket", "manual"] as const).some(
          (pr) => (providerCounts[pr] ?? 0) > 0,
        ) ? (
          <>
            <span className="mx-1 h-4 w-px bg-border" aria-hidden />
            {(["github", "gitlab", "bitbucket", "manual"] as const).map((pr) =>
              (providerCounts[pr] ?? 0) > 0 ? (
                <FilterPill
                  key={pr}
                  label={providerLabel(pr)}
                  count={providerCounts[pr] ?? 0}
                  active={provider === pr}
                  onClick={() => setProvider(provider === pr ? "all" : pr)}
                  tone="neutral"
                  icon={<ProviderIcon provider={pr} className="size-3" />}
                />
              ) : null,
            )}
          </>
        ) : null}
      </div>

      {filtered.length === 0 ? (
        <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
          No projects match the current filter.
        </p>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {filtered.map((p) => (
            <ProjectCard key={p.id} project={p} />
          ))}
        </div>
      )}
    </div>
  );
}

function ProjectCard({ project }: { project: ProjectSummary }) {
  const tone = statusToTone(project.status);
  return (
    <Link
      href={`/projects/${project.slug}` as Route}
      className="group flex flex-col gap-4 rounded-xl border bg-card p-5 shadow-sm transition-colors hover:border-primary/40"
    >
      <div className="flex items-start justify-between gap-2">
        <div className="flex items-center gap-1.5 text-xs font-medium uppercase tracking-wide text-muted-foreground">
          <ProviderIcon provider={project.provider} className="size-4" />
          {project.provider ? providerLabel(project.provider) : "no repo"}
        </div>
        <StatusBadgeCard status={project.status} />
      </div>

      <div className="space-y-1">
        <h3 className="truncate font-mono text-base font-semibold">
          {project.slug}
        </h3>
        <p className="truncate text-sm text-muted-foreground">{project.name}</p>
      </div>

      {project.description ? (
        <p className="line-clamp-2 text-sm text-muted-foreground">
          {project.description}
        </p>
      ) : (
        <p className="text-sm italic text-muted-foreground/70">
          No description.
        </p>
      )}

      {project.top_pipelines && project.top_pipelines.length > 0 ? (
        <div className="space-y-2">
          {project.top_pipelines.map((pl) => (
            <PipelinePreviewRow key={pl.id} pipeline={pl} />
          ))}
          {project.pipeline_count > project.top_pipelines.length ? (
            <p className="pt-0.5 text-xs text-muted-foreground/70">
              +{project.pipeline_count - project.top_pipelines.length} more
            </p>
          ) : null}
        </div>
      ) : null}

      <div className="mt-auto flex items-center justify-between border-t pt-3 text-xs text-muted-foreground">
        <span>
          {project.pipeline_count} pipeline
          {project.pipeline_count === 1 ? "" : "s"} · {project.run_count} run
          {project.run_count === 1 ? "" : "s"}
        </span>
        {project.latest_run_at ? (
          <span>
            <RelativeTime at={project.latest_run_at} />
          </span>
        ) : (
          <span className="italic">Never run</span>
        )}
      </div>

      <span
        className={cn(
          "flex items-center gap-1 text-xs font-medium text-muted-foreground transition-colors",
          "group-hover:text-primary",
          tone === "failed" && "group-hover:text-destructive",
        )}
      >
        Open project <ArrowRight className="size-3.5" aria-hidden />
      </span>
    </Link>
  );
}

function PipelinePreviewRow({ pipeline }: { pipeline: PipelinePreview }) {
  // Merge definition stages with stage_runs (keyed by name). When
  // the pipeline has never run we rely on definition alone — each
  // stage renders as a neutral pending dot. Same pattern as
  // PipelineCard on the project detail page, shrunk for the
  // card-in-grid layout.
  const runStages = pipeline.latest_run_stages ?? [];
  const defStages = pipeline.definition_stages ?? [];
  const runByName = new Map(runStages.map((s) => [s.name, s]));
  const merged: Array<{ name: string; run?: StageRunSummary }> =
    defStages.length > 0
      ? defStages.map((name) => ({ name, run: runByName.get(name) }))
      : runStages.map((s) => ({ name: s.name, run: s }));

  return (
    <div className="flex items-center justify-between gap-3 text-xs">
      <span className="min-w-0 flex-1 truncate font-mono text-foreground">
        {pipeline.name}
      </span>
      {merged.length > 0 ? (
        <div className="flex shrink-0 items-center gap-0.5">
          {merged.map((s, i) => (
            <StageNode
              key={`${s.name}-${i}`}
              name={s.name}
              run={s.run}
              isLast={i === merged.length - 1}
            />
          ))}
        </div>
      ) : (
        <span className="shrink-0 text-[11px] italic text-muted-foreground/70">
          never run
        </span>
      )}
    </div>
  );
}

// StageNode is the GitLab-CI-style status chip: a filled circle
// carrying a status icon, with a thin connector line to the next
// stage. Keeps the stage name out of the visual (title/aria carries
// it) so the strip stays legible inside the project card grid.
function StageNode({
  name,
  run,
  isLast,
}: {
  name: string;
  run?: StageRunSummary;
  isLast: boolean;
}) {
  const tone: StatusTone = run ? statusTone(run.status) : "neutral";
  const label = run ? `${name} — ${run.status}` : `${name} — not run`;
  return (
    <div className="flex items-center">
      <span
        title={label}
        aria-label={label}
        className={cn(
          "inline-flex size-5 items-center justify-center rounded-full",
          stageNodeClasses[tone],
          run?.status === "running" && "animate-pulse",
        )}
      >
        <StageIcon tone={tone} />
      </span>
      {!isLast ? (
        <span
          aria-hidden
          className="inline-block h-px w-1.5 bg-muted-foreground/30"
        />
      ) : null}
    </div>
  );
}

function StageIcon({ tone }: { tone: StatusTone }) {
  const shared = "size-3";
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

function FilterPill({
  label,
  count,
  active,
  onClick,
  tone,
  icon,
}: {
  label: string;
  count: number;
  active: boolean;
  onClick: () => void;
  tone: "all" | StatusTone;
  icon?: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 font-medium transition-colors",
        active
          ? activePillClasses[tone]
          : "border-border bg-background text-muted-foreground hover:border-foreground/30 hover:text-foreground",
      )}
    >
      {icon}
      <span>{label}</span>
      <span
        className={cn(
          "rounded-full px-1.5 text-[10px]",
          active ? "bg-background/40" : "bg-muted",
        )}
      >
        {count}
      </span>
    </button>
  );
}

function StatusBadgeCard({ status }: { status: ProjectStatus }) {
  const tone = statusToTone(status);
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[10px] font-medium",
        tonePillClasses[tone],
      )}
    >
      <StatusIcon status={status} />
      {statusLabel(status)}
    </span>
  );
}

function StatusIcon({ status }: { status: ProjectStatus }) {
  switch (status) {
    case "running":
      return <Loader2 className="size-2.5 animate-spin" aria-hidden />;
    case "success":
      return <CheckCircle2 className="size-2.5" aria-hidden />;
    case "failing":
      return <XCircle className="size-2.5" aria-hidden />;
    default:
      return <CircleDashed className="size-2.5" aria-hidden />;
  }
}

function ToneDot({ tone, running }: { tone: StatusTone; running: boolean }) {
  return (
    <span
      className={cn(
        "inline-block size-1.5 rounded-full",
        dotClasses[tone],
        running && "animate-pulse",
      )}
      aria-hidden
    />
  );
}

function ProviderIcon({
  provider,
  className,
}: {
  provider?: ProjectProvider;
  className?: string;
}) {
  // lucide-react in this version doesn't ship brand marks for
  // GitHub/GitLab — `GitBranch` is the closest semantic icon and
  // keeps the bundle small. The provider name next to it carries
  // the actual branding signal anyway.
  if (!provider) return <CircleDashed className={className} aria-hidden />;
  return <GitBranch className={className} aria-hidden />;
}

function countBy<T, K extends string>(
  items: T[],
  keyFn: (item: T) => K,
): Record<K, number> {
  const out = {} as Record<K, number>;
  for (const item of items) {
    const k = keyFn(item);
    out[k] = (out[k] ?? 0) + 1;
  }
  return out;
}

function statusLabel(s: ProjectStatus): string {
  switch (s) {
    case "no_pipelines":
      return "No pipelines";
    case "never_run":
      return "Never run";
    case "running":
      return "Running";
    case "failing":
      return "Failing";
    case "success":
      return "Healthy";
  }
}

function statusToTone(s: ProjectStatus): StatusTone {
  switch (s) {
    case "running":
      return "running";
    case "success":
      return "success";
    case "failing":
      return "failed";
    case "never_run":
    case "no_pipelines":
      return "neutral";
  }
}

function providerLabel(p: ProjectProvider): string {
  if (!p) return "No repo";
  return p.charAt(0).toUpperCase() + p.slice(1);
}

const tonePillClasses: Record<StatusTone, string> = {
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

// StageNode fill colours — GitLab-style filled circle, white icon
// inside. Neutral / skipped use a paler ring + muted icon so "not
// run" still reads as inactive next to a green success chip.
const stageNodeClasses: Record<StatusTone, string> = {
  success: "bg-emerald-500 text-white",
  failed: "bg-red-500 text-white",
  running: "bg-sky-500 text-white",
  queued: "bg-amber-500 text-white",
  warning: "bg-amber-500 text-white",
  canceled: "bg-muted-foreground/60 text-background",
  skipped: "bg-muted text-muted-foreground border border-muted-foreground/30",
  neutral: "bg-muted text-muted-foreground border border-muted-foreground/30",
};

const dotClasses: Record<StatusTone, string> = {
  success: "bg-emerald-500",
  failed: "bg-red-500",
  running: "bg-sky-500",
  queued: "bg-amber-500",
  warning: "bg-amber-500",
  canceled: "bg-muted-foreground",
  skipped: "bg-muted-foreground/60",
  neutral: "bg-muted-foreground/40",
};

const activePillClasses: Record<"all" | StatusTone, string> = {
  all: "border-primary bg-primary text-primary-foreground",
  success:
    "border-emerald-500/50 bg-emerald-500/15 text-emerald-700 dark:text-emerald-400",
  failed: "border-red-500/50 bg-red-500/15 text-red-700 dark:text-red-400",
  running: "border-sky-500/50 bg-sky-500/15 text-sky-700 dark:text-sky-400",
  queued:
    "border-amber-500/50 bg-amber-500/15 text-amber-700 dark:text-amber-400",
  warning:
    "border-amber-500/50 bg-amber-500/15 text-amber-700 dark:text-amber-400",
  canceled: "border-foreground/40 bg-muted text-foreground",
  skipped: "border-foreground/30 bg-muted text-foreground",
  neutral: "border-foreground/40 bg-muted text-foreground",
};
