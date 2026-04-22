"use client";

import { useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { RefreshCw, Search } from "lucide-react";

import { cn } from "@/lib/utils";
import { Input } from "@/components/ui/input";
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
  const activeCount = useMemo(() => countActive(pipelines, runs), [pipelines, runs]);
  useLiveRefresh(activeCount > 0);

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

        <div className="relative ml-auto w-full sm:w-64">
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
            className="h-8 pl-8 text-xs"
          />
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
        />
      )}
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
        "inline-flex h-8 items-center gap-1.5 rounded-full border px-3 text-xs font-medium transition-colors",
        active
          ? "border-foreground/20 bg-foreground text-background"
          : "border-border bg-background text-muted-foreground hover:bg-accent hover:text-foreground",
      )}
    >
      {tone ? (
        <span
          aria-hidden
          className={cn("size-1.5 rounded-full", toneDotClasses[tone])}
        />
      ) : null}
      {label}
      <span
        className={cn(
          "rounded-full px-1.5 text-[10px] tabular-nums",
          active
            ? "bg-background/20 text-background"
            : "bg-muted text-muted-foreground",
        )}
      >
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

// countActive treats a pipeline as active when either its latest
// run is still non-terminal OR any recent run on this project is.
// The second check catches the "new run just got kicked off" case
// before the latest_run swap materialises in the backend's
// DISTINCT ON snapshot.
function countActive(
  pipelines: PipelineSummary[],
  runs: RunSummary[],
): number {
  let n = 0;
  for (const p of pipelines) {
    if (isActiveStatus(p.latest_run?.status)) n++;
  }
  for (const r of runs) {
    if (isActiveStatus(r.status)) n++;
  }
  return n;
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
