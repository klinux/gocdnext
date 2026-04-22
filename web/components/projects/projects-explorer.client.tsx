"use client";

import { useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import {
  CircleDashed,
  GitBranch,
  LayoutGrid,
  List,
  RefreshCw,
  Search,
} from "lucide-react";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { FilterPill } from "@/components/projects/project-filter-pills";
import { ProjectCard } from "@/components/projects/project-card";
import { ProjectRow } from "@/components/projects/project-row";
import { VisibleProjectsMenu } from "@/components/projects/visible-projects-menu.client";
import {
  countBy,
  providerLabel,
  statusLabel,
  statusToTone,
} from "@/components/projects/project-ui-helpers";
import type {
  ProjectProvider,
  ProjectStatus,
  ProjectSummary,
} from "@/types/api";

type Props = {
  projects: ProjectSummary[];
  initialHiddenProjects: string[];
};
type ViewMode = "grid" | "list";
const VIEW_STORAGE_KEY = "gocdnext.projects.view";

// ProjectsExplorer owns the toolbar (search + filter pills + view
// toggle) and swaps between the grid/list views. State is all
// client-side — dataset is small enough (dozens of projects) that
// filtering in memory beats a round-trip. View choice is persisted
// to localStorage so the user's preference survives navigation.
export function ProjectsExplorer({ projects, initialHiddenProjects }: Props) {
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState<ProjectStatus | "all">("all");
  const [provider, setProvider] = useState<ProjectProvider | "all">("all");
  const [view, setView] = useState<ViewMode>("grid");
  // Local mirror of the hide-list — the menu writes here on every
  // toggle for instant UX, while the debounced save persists to the
  // server in the background. Server value flows back on refresh.
  const [hiddenProjects, setHiddenProjects] =
    useState<string[]>(initialHiddenProjects);
  const activeCount = useMemo(() => countActive(projects), [projects]);
  useLiveRefresh(activeCount > 0);

  useEffect(() => {
    const stored = window.localStorage.getItem(VIEW_STORAGE_KEY);
    if (stored === "grid" || stored === "list") setView(stored);
  }, []);

  const setViewAndPersist = (next: ViewMode) => {
    setView(next);
    window.localStorage.setItem(VIEW_STORAGE_KEY, next);
  };

  // Hide-list is applied before counts + filter so the status pills
  // (`Running 2`, `Failing 1`) reflect what the user actually sees,
  // not the whole org. Keeps the pill numbers honest vs. the grid.
  const visibleProjects = useMemo(() => {
    if (hiddenProjects.length === 0) return projects;
    const hide = new Set(hiddenProjects);
    return projects.filter((p) => !hide.has(p.id));
  }, [projects, hiddenProjects]);

  const statusCounts = useMemo(
    () => countBy(visibleProjects, (p) => p.status),
    [visibleProjects],
  );
  const providerCounts = useMemo(
    () => countBy(visibleProjects, (p) => p.provider ?? ""),
    [visibleProjects],
  );

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return visibleProjects.filter((p) => {
      if (status !== "all" && p.status !== status) return false;
      if (provider !== "all" && (p.provider ?? "") !== provider) return false;
      if (!q) return true;
      return (
        p.slug.toLowerCase().includes(q) ||
        p.name.toLowerCase().includes(q) ||
        (p.description ?? "").toLowerCase().includes(q)
      );
    });
  }, [visibleProjects, query, status, provider]);

  return (
    <div className="space-y-5">
      <div className="flex flex-col gap-3 lg:flex-row lg:items-center">
        <div className="relative flex-1">
          <Search
            className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            value={query}
            onValueChange={(next: string) => setQuery(next)}
            placeholder="Search by slug, name or description..."
            className="h-10 pl-9 text-sm"
            aria-label="Search projects"
          />
        </div>
        {activeCount > 0 ? (
          <span
            className="inline-flex items-center gap-1.5 text-xs text-sky-500"
            title="Auto-refreshing while runs are active"
          >
            <RefreshCw className="size-3 animate-spin" aria-hidden />
            live · {activeCount} active
          </span>
        ) : null}
        <VisibleProjectsMenu
          projects={projects}
          initialHidden={hiddenProjects}
          onLocalChange={setHiddenProjects}
        />
        <ViewToggle view={view} onChange={setViewAndPersist} />
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <FilterPill
          label="All"
          count={visibleProjects.length}
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
      ) : view === "grid" ? (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {filtered.map((p) => (
            <ProjectCard key={p.id} project={p} />
          ))}
        </div>
      ) : (
        <div className="flex flex-col gap-2">
          {filtered.map((p) => (
            <ProjectRow key={p.id} project={p} />
          ))}
        </div>
      )}
    </div>
  );
}

function ViewToggle({
  view,
  onChange,
}: {
  view: ViewMode;
  onChange: (v: ViewMode) => void;
}) {
  return (
    <div className="inline-flex shrink-0 rounded-md border border-border bg-background p-0.5">
      <button
        type="button"
        onClick={() => onChange("grid")}
        aria-pressed={view === "grid"}
        aria-label="Grid view"
        className={cn(
          "inline-flex items-center gap-1 rounded px-2.5 py-1 text-xs font-medium transition-colors",
          view === "grid"
            ? "bg-foreground text-background"
            : "text-muted-foreground hover:text-foreground",
        )}
      >
        <LayoutGrid className="size-3.5" aria-hidden />
        Grid
      </button>
      <button
        type="button"
        onClick={() => onChange("list")}
        aria-pressed={view === "list"}
        aria-label="List view"
        className={cn(
          "inline-flex items-center gap-1 rounded px-2.5 py-1 text-xs font-medium transition-colors",
          view === "list"
            ? "bg-foreground text-background"
            : "text-muted-foreground hover:text-foreground",
        )}
      >
        <List className="size-3.5" aria-hidden />
        List
      </button>
    </div>
  );
}

function ProviderIcon({
  provider,
  className,
}: {
  provider?: ProjectProvider;
  className?: string;
}) {
  // lucide-react doesn't ship brand marks in this version —
  // GitBranch is the closest generic "repo" glyph. The provider
  // label next to it carries the real branding signal.
  if (!provider) return <CircleDashed className={className} aria-hidden />;
  return <GitBranch className={className} aria-hidden />;
}

// countActive sweeps the project list for anything non-terminal —
// if any project has a running/queued pipeline, the page should
// poll so the user doesn't have to F5 while a build is in flight.
function countActive(projects: ProjectSummary[]): number {
  let n = 0;
  for (const p of projects) {
    if (p.status === "running") {
      n++;
      continue;
    }
    // status="success"/"failing"/"never_run" are terminal from a
    // refresh-need perspective; only "running" warrants polling
    // at the project scope. The pipeline-level live indicator on
    // the detail page handles per-pipeline queued states.
  }
  return n;
}

// useLiveRefresh polls router.refresh() every 3s while anything
// is active. Stops the interval as soon as the active count
// drops to zero, then fires one last refresh so the final status
// lands in the UI without requiring a manual F5.
function useLiveRefresh(active: boolean) {
  const router = useRouter();
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => router.refresh(), 3000);
    return () => {
      clearInterval(id);
      router.refresh();
    };
  }, [active, router]);
}
