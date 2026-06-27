"use client";

import { useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { List, Network, RefreshCw, Search } from "lucide-react";

import { cn } from "@/lib/utils";
import { Input } from "@/components/ui/input";
import { DurationTrendPill } from "@/components/shared/duration-trend-pill.client";
import { runDurationPoints } from "@/components/shared/duration-trend";
import { PipelineFlow } from "@/components/pipelines/pipeline-flow";
import type {
  PipelineEdge,
  PipelineSummary,
  RunSummary,
} from "@/types/api";

type Props = {
  projectSlug: string;
  pipelines: PipelineSummary[];
  edges: PipelineEdge[];
  runs: RunSummary[];
};

type StatusFilter = "all" | "running" | "failing" | "passing" | "never";
type FlowView = "flow" | "flat";

// View choice persists to localStorage — it's an ephemeral display
// preference (like the projects grid/list toggle), not account data, so
// a round-trip to user_preferences would be overkill.
const VIEW_STORAGE_KEY = "gocdnext.pipelines.view";

// Filter pills live on the Pipelines tab so a user staring at 20+
// pipelines can narrow to what matters (what's red, what's moving)
// without digging through cards. Search is substring by name —
// the pipeline count makes a proper fuzzy match overkill.
export function PipelineFlowExplorer({
  projectSlug,
  pipelines,
  edges,
  runs,
}: Props) {
  const [status, setStatus] = useState<StatusFilter>("all");
  const [query, setQuery] = useState("");
  const [view, setView] = useState<FlowView>("flow");
  const activeCount = useMemo(() => countActive(pipelines, runs), [pipelines, runs]);
  // Project-wide trend for the toolbar pill: every pipeline's recent run
  // durations together, ordered by finish time (counter is per-pipeline, so
  // it can't sort the mixed series). withPipeline keeps labels unambiguous.
  const pillPoints = useMemo(
    () => runDurationPoints(runs, 30, { withPipeline: true }),
    [runs],
  );
  useLiveRefresh(activeCount > 0);

  useEffect(() => {
    const stored = window.localStorage.getItem(VIEW_STORAGE_KEY);
    // eslint-disable-next-line react-hooks/set-state-in-effect -- localStorage is client-only; reading it in a lazy initializer would diverge from the server's "flow" render and cause a hydration mismatch, so the persisted choice is applied here after mount
    if (stored === "flow" || stored === "flat") setView(stored);
  }, []);

  const setViewAndPersist = (next: FlowView) => {
    setView(next);
    window.localStorage.setItem(VIEW_STORAGE_KEY, next);
  };

  const counts = useMemo(() => countByStatus(pipelines), [pipelines]);

  const filtered = useMemo(
    () => filterPipelines(pipelines, status, query.trim().toLowerCase()),
    [pipelines, status, query],
  );
  const filteredNames = useMemo(
    () => new Set(filtered.map((p) => p.name)),
    [filtered],
  );
  // Edges with one endpoint filtered out become orphan arrows —
  // drop them so the DAG doesn't draw lines into empty space.
  const filteredEdges = useMemo(
    () =>
      edges.filter(
        (e) =>
          filteredNames.has(e.from_pipeline) &&
          filteredNames.has(e.to_pipeline),
      ),
    [edges, filteredNames],
  );

  const hasFilter = status !== "all" || query.trim() !== "";
  const total = pipelines.length;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <div className="inline-flex flex-wrap items-center rounded-lg border border-border bg-card p-1">
          <Pill
            label="All"
            count={total}
            active={status === "all"}
            onClick={() => setStatus("all")}
          />
          <Pill
            label="Running"
            tone="running"
            count={counts.running}
            active={status === "running"}
            onClick={() => setStatus("running")}
          />
          <Pill
            label="Failing"
            tone="failed"
            count={counts.failing}
            active={status === "failing"}
            onClick={() => setStatus("failing")}
          />
          <Pill
            label="Passing"
            tone="success"
            count={counts.passing}
            active={status === "passing"}
            onClick={() => setStatus("passing")}
          />
          <Pill
            label="Never run"
            tone="neutral"
            count={counts.never}
            active={status === "never"}
            onClick={() => setStatus("never")}
          />
        </div>

        <FlowToggle view={view} onChange={setViewAndPersist} />

        <div className="ml-auto flex flex-wrap items-center justify-end gap-2">
          <DurationTrendPill
            points={pillPoints}
            note="across all pipelines"
            className="hidden sm:block"
          />
          <div className="relative w-full sm:w-64">
            <Search
              aria-hidden
              className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
            />
            <Input
              type="search"
              placeholder="Search pipelines"
              value={query}
              onValueChange={(v) => setQuery(v)}
              aria-label="Search pipelines by name"
              className="h-9 pl-8 text-xs"
            />
          </div>
        </div>
      </div>

      <div className="flex items-center gap-3 text-xs text-muted-foreground">
        {hasFilter ? (
          <p>
            Showing{" "}
            <span className="font-semibold text-foreground">
              {filtered.length}
            </span>{" "}
            of {total} pipelines
          </p>
        ) : null}
        {activeCount > 0 ? (
          <span
            className="ml-auto inline-flex items-center gap-1.5 text-sky-500"
            title="Auto-refreshing while runs are active"
          >
            <RefreshCw className="size-3 animate-spin" aria-hidden />
            <span>
              live · {activeCount} active run
              {activeCount === 1 ? "" : "s"}
            </span>
          </span>
        ) : null}
      </div>

      {filtered.length === 0 ? (
        <div className="rounded-md border border-dashed border-border p-8 text-center text-sm text-muted-foreground">
          No pipelines match these filters.
        </div>
      ) : (
        <PipelineFlow
          projectSlug={projectSlug}
          pipelines={filtered}
          edges={filteredEdges}
          runs={runs}
          view={view}
        />
      )}
    </div>
  );
}

// FlowToggle switches between the dependency-grouped "Flow" view and the
// flat "List" view. Mirrors the projects grid/list toggle's look.
function FlowToggle({
  view,
  onChange,
}: {
  view: FlowView;
  onChange: (v: FlowView) => void;
}) {
  return (
    <div className="inline-flex shrink-0 items-center rounded-lg border border-border bg-card p-1">
      <button
        type="button"
        onClick={() => onChange("flow")}
        aria-pressed={view === "flow"}
        aria-label="Flow view"
        className={cn(
          "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-xs font-medium transition-colors",
          view === "flow"
            ? "bg-primary/15 text-primary"
            : "text-muted-foreground hover:text-foreground",
        )}
      >
        <Network className="size-3.5" aria-hidden />
        Flow
      </button>
      <button
        type="button"
        onClick={() => onChange("flat")}
        aria-pressed={view === "flat"}
        aria-label="List view"
        className={cn(
          "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-xs font-medium transition-colors",
          view === "flat"
            ? "bg-primary/15 text-primary"
            : "text-muted-foreground hover:text-foreground",
        )}
      >
        <List className="size-3.5" aria-hidden />
        List
      </button>
    </div>
  );
}

type PillProps = {
  label: string;
  count: number;
  active: boolean;
  onClick: () => void;
  tone?: "running" | "failed" | "success" | "neutral";
};

function Pill({ label, count, active, onClick, tone }: PillProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-xs font-medium transition-colors",
        active
          ? "bg-accent text-foreground"
          : "text-muted-foreground hover:text-foreground",
      )}
    >
      {tone ? (
        <span
          aria-hidden
          className={cn("size-[7px] rounded-full", toneDotClasses[tone])}
        />
      ) : null}
      {label}
      <span className="text-[11px] font-semibold tabular-nums text-muted-foreground">
        {count}
      </span>
    </button>
  );
}

const toneDotClasses: Record<NonNullable<PillProps["tone"]>, string> = {
  running: "bg-sky-500",
  failed: "bg-red-500",
  success: "bg-emerald-500",
  neutral: "bg-muted-foreground/50",
};

function countByStatus(pipelines: PipelineSummary[]) {
  let running = 0;
  let failing = 0;
  let passing = 0;
  let never = 0;
  for (const p of pipelines) {
    const s = p.latest_run?.status;
    if (!s) {
      never++;
      continue;
    }
    if (s === "running" || s === "queued" || s === "waiting") running++;
    else if (s === "failed") failing++;
    else if (s === "success") passing++;
  }
  return { running, failing, passing, never };
}

// countActive returns the number of distinct active runs across the
// project. Two sources can each surface the same run: a pipeline's
// `latest_run` snapshot AND the recent `runs` list. Union them by
// run id so a run mid-flight isn't double-counted. The recent runs
// list also catches the "new run just got kicked off" case before
// the latest_run swap materialises in the backend's DISTINCT ON
// snapshot — without dedupe, that overlap is what was inflating the
// count.
function countActive(
  pipelines: PipelineSummary[],
  runs: RunSummary[],
): number {
  const active = new Set<string>();
  for (const p of pipelines) {
    const r = p.latest_run;
    if (r && isActiveStatus(r.status)) active.add(r.id);
  }
  for (const r of runs) {
    if (isActiveStatus(r.status)) active.add(r.id);
  }
  return active.size;
}

function isActiveStatus(s: string | undefined | null): boolean {
  return s === "running" || s === "queued" || s === "waiting";
}

// useLiveRefresh polls router.refresh() every 3s while anything is
// active. Stops the interval as soon as the active count drops to
// zero, then fires one last refresh so the final terminal status
// lands in the UI without the user F5-ing. The page is RSC +
// force-dynamic, so refresh() re-runs getProjectDetail.
function useLiveRefresh(active: boolean) {
  const router = useRouter();
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => router.refresh(), 3000);
    return () => {
      clearInterval(id);
      // Final kick so the moment-of-completion status replaces the
      // last "running" snapshot — otherwise the UI only refreshes
      // on the next user interaction.
      router.refresh();
    };
  }, [active, router]);
}

function filterPipelines(
  pipelines: PipelineSummary[],
  status: StatusFilter,
  query: string,
): PipelineSummary[] {
  return pipelines.filter((p) => {
    if (query && !p.name.toLowerCase().includes(query)) return false;
    if (status === "all") return true;
    const s = p.latest_run?.status;
    if (status === "never") return !s;
    if (status === "running") {
      return s === "running" || s === "queued" || s === "waiting";
    }
    if (status === "failing") return s === "failed";
    if (status === "passing") return s === "success";
    return true;
  });
}
