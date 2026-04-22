"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import {
  ArrowRight,
  Download,
  FileCode,
  GitBranch,
  Loader2,
  TrendingUp,
} from "lucide-react";

import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { LiveDuration } from "@/components/shared/live-duration";
import { cn } from "@/lib/utils";
import { env } from "@/lib/env";
import { formatDurationSeconds } from "@/lib/format";
import type {
  PipelineEdge,
  PipelineSummary,
  RunArtifact,
  RunSummary,
} from "@/types/api";

type Props = {
  open: boolean;
  onOpenChange: (next: boolean) => void;
  pipeline: PipelineSummary;
  edges: PipelineEdge[];
  runs: RunSummary[];
};

type TabKey = "overview" | "runs" | "triggers" | "artifacts" | "yaml";

// PipelineOverviewSheet is the slide-in companion with tabbed
// content: Overview (KPIs + details + per-stage), Runs (recent
// runs), Triggers (upstream/downstream), Artifacts (lazy fetch
// for latest run), YAML (reconstructed from definition snapshot
// — the raw YAML isn't persisted today, only the parsed JSONB).
export function PipelineOverviewSheet({
  open,
  onOpenChange,
  pipeline,
  edges,
  runs,
}: Props) {
  const [tab, setTab] = useState<TabKey>("overview");
  const run = pipeline.latest_run;
  const meta = pipeline.latest_run_meta;

  const upstream = useMemo(
    () => edges.filter((e) => e.to_pipeline === pipeline.name),
    [edges, pipeline.name],
  );
  const downstream = useMemo(
    () => edges.filter((e) => e.from_pipeline === pipeline.name),
    [edges, pipeline.name],
  );
  const recentRuns = useMemo(
    () => runs.filter((r) => r.pipeline_id === pipeline.id).slice(0, 25),
    [runs, pipeline.id],
  );

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex flex-col gap-0 p-0 data-[side=right]:w-[560px] data-[side=right]:sm:max-w-[560px]"
      >
        <SheetHeader className="border-b border-border px-5 pt-5 pb-4">
          <SheetDescription className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
            Pipeline
          </SheetDescription>
          <SheetTitle className="truncate font-mono text-lg">
            {pipeline.name}
          </SheetTitle>
          <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted-foreground">
            {run ? (
              <>
                <span className="font-mono">#{run.counter}</span>
                <StatusBadge status={run.status} className="text-[10px]" />
                <LiveDuration
                  startedAt={run.started_at}
                  finishedAt={run.finished_at}
                />
                <span>
                  · started{" "}
                  <RelativeTime at={run.started_at ?? run.created_at} />
                </span>
              </>
            ) : (
              <span className="italic">Never run</span>
            )}
          </div>
        </SheetHeader>

        <Tabs
          value={tab}
          onValueChange={(next) => setTab(next as TabKey)}
          className="flex flex-1 flex-col gap-0 overflow-hidden"
        >
          <TabsList
            variant="line"
            className="w-full justify-start gap-3 rounded-none border-b border-border px-5"
          >
            <TabsTrigger value="overview">overview</TabsTrigger>
            <TabsTrigger value="runs">runs</TabsTrigger>
            <TabsTrigger value="triggers">
              triggers
              {upstream.length + downstream.length > 0 ? (
                <span className="ml-1 text-[10px] text-muted-foreground/70">
                  {upstream.length + downstream.length}
                </span>
              ) : null}
            </TabsTrigger>
            <TabsTrigger value="artifacts">artifacts</TabsTrigger>
            <TabsTrigger value="yaml">yaml</TabsTrigger>
          </TabsList>

          <TabsContent
            value="overview"
            className="flex-1 overflow-y-auto px-5 py-4"
          >
            <OverviewPanel pipeline={pipeline} meta={meta} />
          </TabsContent>

          <TabsContent
            value="runs"
            className="flex-1 overflow-y-auto px-5 py-4"
          >
            <RunsPanel runs={recentRuns} />
          </TabsContent>

          <TabsContent
            value="triggers"
            className="flex-1 overflow-y-auto px-5 py-4"
          >
            <TriggersPanel
              upstream={upstream}
              downstream={downstream}
              pipelineName={pipeline.name}
            />
          </TabsContent>

          <TabsContent
            value="artifacts"
            className="flex-1 overflow-y-auto px-5 py-4"
          >
            <ArtifactsPanel runId={run?.id} active={tab === "artifacts"} />
          </TabsContent>

          <TabsContent
            value="yaml"
            className="flex-1 overflow-y-auto px-5 py-4"
          >
            <YAMLPanel pipeline={pipeline} />
          </TabsContent>
        </Tabs>
      </SheetContent>
    </Sheet>
  );
}

function OverviewPanel({
  pipeline,
  meta,
}: {
  pipeline: PipelineSummary;
  meta: PipelineSummary["latest_run_meta"];
}) {
  const metrics = pipeline.metrics;
  const run = pipeline.latest_run;
  return (
    <div className="space-y-5">
      <section className="grid grid-cols-3 gap-2">
        <Kpi
          label="Success (7d)"
          value={
            metrics && metrics.runs_considered > 0
              ? `${Math.round(metrics.success_rate * 100)}%`
              : "—"
          }
          toneFrom={metrics?.success_rate}
        />
        <Kpi
          label="P50 duration"
          value={
            metrics && metrics.runs_considered > 0
              ? formatDurationSeconds(metrics.lead_time_p50_seconds)
              : "—"
          }
        />
        <Kpi
          label="Runs / day"
          value={
            metrics && metrics.runs_considered > 0 && metrics.window_days > 0
              ? (metrics.runs_considered / metrics.window_days).toFixed(1)
              : "—"
          }
          hint={
            metrics && metrics.runs_considered > 0
              ? `${metrics.runs_considered} in ${metrics.window_days}d`
              : undefined
          }
        />
      </section>

      <Section title="Details">
        <DefList>
          {run ? (
            <Row label="triggered by">
              <span className="truncate">
                {run.triggered_by || run.cause}
                {run.cause && run.triggered_by ? (
                  <span className="text-muted-foreground"> · {run.cause}</span>
                ) : null}
              </span>
            </Row>
          ) : null}
          {meta?.branch || meta?.revision ? (
            <Row label="ref">
              <span className="inline-flex max-w-full items-center gap-1.5 truncate">
                {meta.branch ? (
                  <span className="inline-flex items-center gap-0.5 rounded bg-muted px-1.5 py-0.5 font-mono text-[10px]">
                    <GitBranch className="size-3" aria-hidden />
                    {meta.branch}
                  </span>
                ) : null}
                {meta.revision ? (
                  <span className="truncate font-mono text-[11px] text-muted-foreground">
                    {meta.revision.slice(0, 7)}
                  </span>
                ) : null}
              </span>
            </Row>
          ) : null}
          {meta?.message ? (
            <Row label="commit">
              <span className="truncate" title={meta.message}>
                {meta.message}
              </span>
            </Row>
          ) : null}
          {meta?.author ? <Row label="author">{meta.author}</Row> : null}
          {run?.started_at ? (
            <Row label="started">
              <span className="font-mono text-[11px]">
                {new Date(run.started_at).toLocaleString(undefined, {
                  hour: "2-digit",
                  minute: "2-digit",
                  second: "2-digit",
                  day: "2-digit",
                  month: "short",
                })}
              </span>
            </Row>
          ) : null}
          <Row label="definition">v{pipeline.definition_version}</Row>
        </DefList>
      </Section>

      {metrics?.stage_stats && metrics.stage_stats.length > 0 ? (
        <Section title="Per-stage (7d)">
          <ul className="space-y-1">
            {metrics.stage_stats.map((s) => (
              <li
                key={s.name}
                className="flex items-center justify-between rounded-md border border-border bg-background px-2 py-1.5 text-xs"
              >
                <span className="font-mono text-[11px] uppercase tracking-wide text-muted-foreground">
                  {s.name}
                </span>
                <span className="inline-flex items-center gap-2 font-mono text-[11px] tabular-nums">
                  <span title="p50 duration">
                    {formatDurationSeconds(s.duration_p50_seconds)}
                  </span>
                  <span
                    className={cn(
                      "rounded px-1 text-[10px] font-medium",
                      passRateTone(s.success_rate),
                    )}
                  >
                    ✓ {Math.round(s.success_rate * 100)}%
                  </span>
                </span>
              </li>
            ))}
          </ul>
        </Section>
      ) : null}
    </div>
  );
}

function RunsPanel({ runs }: { runs: RunSummary[] }) {
  if (runs.length === 0) {
    return (
      <p className="text-xs italic text-muted-foreground">
        No runs yet for this pipeline.
      </p>
    );
  }
  return (
    <ul className="space-y-1.5">
      {runs.map((r) => (
        <RecentRunRow key={r.id} run={r} />
      ))}
    </ul>
  );
}

function TriggersPanel({
  upstream,
  downstream,
  pipelineName,
}: {
  upstream: PipelineEdge[];
  downstream: PipelineEdge[];
  pipelineName: string;
}) {
  if (upstream.length === 0 && downstream.length === 0) {
    return (
      <p className="text-xs italic text-muted-foreground">
        {pipelineName} has no upstream or downstream relationships yet.
      </p>
    );
  }
  return (
    <div className="space-y-5">
      {upstream.length > 0 ? (
        <Section title="Triggered by">
          <ul className="space-y-1.5">
            {upstream.map((e) => (
              <EdgeRow
                key={`u-${e.from_pipeline}-${e.stage ?? ""}-${e.status ?? ""}`}
                pipelineName={e.from_pipeline}
                hint={edgeHint(e)}
                direction="in"
              />
            ))}
          </ul>
        </Section>
      ) : null}
      {downstream.length > 0 ? (
        <Section title="Triggers">
          <ul className="space-y-1.5">
            {downstream.map((e) => (
              <EdgeRow
                key={`d-${e.to_pipeline}-${e.stage ?? ""}-${e.status ?? ""}`}
                pipelineName={e.to_pipeline}
                hint={edgeHint(e)}
                direction="out"
              />
            ))}
          </ul>
        </Section>
      ) : null}
    </div>
  );
}

function ArtifactsPanel({
  runId,
  active,
}: {
  runId: string | undefined;
  active: boolean;
}) {
  const [state, setState] = useState<
    | { kind: "idle" }
    | { kind: "loading" }
    | { kind: "ok"; items: RunArtifact[] }
    | { kind: "error"; message: string }
  >({ kind: "idle" });

  useEffect(() => {
    // Lazy-fetch: only hit the API once the user opens this tab.
    // runId missing = pipeline has never run, skip entirely.
    if (!active || !runId) return;
    if (state.kind !== "idle") return;
    let cancelled = false;
    setState({ kind: "loading" });
    fetch(
      `${env.GOCDNEXT_API_URL.replace(/\/+$/, "")}/api/v1/runs/${encodeURIComponent(runId)}/artifacts`,
      { cache: "no-store", credentials: "include" },
    )
      .then(async (res) => {
        if (cancelled) return;
        if (!res.ok) {
          setState({ kind: "error", message: `HTTP ${res.status}` });
          return;
        }
        const body = (await res.json()) as { artifacts?: RunArtifact[] };
        setState({ kind: "ok", items: body.artifacts ?? [] });
      })
      .catch((err) => {
        if (cancelled) return;
        setState({
          kind: "error",
          message: err instanceof Error ? err.message : String(err),
        });
      });
    return () => {
      cancelled = true;
    };
  }, [active, runId, state.kind]);

  if (!runId) {
    return (
      <p className="text-xs italic text-muted-foreground">
        No run yet — artifacts show up after the first execution.
      </p>
    );
  }
  if (state.kind === "loading" || state.kind === "idle") {
    return (
      <p className="inline-flex items-center gap-2 text-xs text-muted-foreground">
        <Loader2 className="size-3 animate-spin" aria-hidden />
        Loading artifacts…
      </p>
    );
  }
  if (state.kind === "error") {
    return (
      <p className="text-xs text-red-500">
        Couldn&apos;t load artifacts: {state.message}
      </p>
    );
  }
  if (state.items.length === 0) {
    return (
      <p className="text-xs italic text-muted-foreground">
        This run produced no artifacts.
      </p>
    );
  }
  return (
    <ul className="space-y-1">
      {state.items.map((a) => (
        <li
          key={a.id}
          className="flex items-center gap-2 rounded-md border border-border bg-background px-2.5 py-1.5"
        >
          <div className="min-w-0 flex-1">
            <p className="truncate font-mono text-xs" title={a.path}>
              {a.path}
            </p>
            <p className="text-[10px] text-muted-foreground">
              <span className="font-mono">{a.job_name}</span> ·{" "}
              {formatBytes(a.size_bytes)}
              {a.expires_at ? (
                <>
                  {" "}· expires{" "}
                  <RelativeTime at={a.expires_at} fallback="—" />
                </>
              ) : null}
            </p>
          </div>
          {a.download_url ? (
            <a
              href={a.download_url}
              className="inline-flex items-center gap-1 rounded bg-muted px-2 py-1 text-[10px] font-medium text-foreground hover:bg-accent"
              title={
                a.download_url_expires_at
                  ? `Link expires ${new Date(a.download_url_expires_at).toLocaleString()}`
                  : "Download artifact"
              }
            >
              <Download className="size-3" aria-hidden />
              download
            </a>
          ) : (
            <span className="text-[10px] italic text-muted-foreground">
              {a.status}
            </span>
          )}
        </li>
      ))}
    </ul>
  );
}

function YAMLPanel({ pipeline }: { pipeline: PipelineSummary }) {
  // The raw YAML isn't persisted today — only the parsed JSONB
  // in pipelines.definition. Reconstruct a canonical pseudo-YAML
  // from the stages + jobs we already have so the tab shows
  // *something* meaningful. When the day comes that the original
  // YAML is stored (or re-fetched from the scm_source), this
  // panel swaps to the real thing.
  const yaml = useMemo(() => reconstructYAML(pipeline), [pipeline]);
  return (
    <div className="space-y-2">
      <p className="inline-flex items-center gap-1.5 text-[11px] text-muted-foreground">
        <FileCode className="size-3.5" aria-hidden />
        Reconstructed from v{pipeline.definition_version} — the original YAML
        isn&apos;t stored yet.
      </p>
      <pre className="overflow-auto rounded-md border border-border bg-muted/30 p-3 font-mono text-[11px] leading-5 text-foreground">
        {yaml}
      </pre>
    </div>
  );
}

function reconstructYAML(pipeline: PipelineSummary): string {
  const lines: string[] = [];
  lines.push(`name: ${pipeline.name}`);
  lines.push(`version: ${pipeline.definition_version}`);
  const stages = pipeline.definition_stages ?? [];
  if (stages.length > 0) {
    lines.push("stages:");
    for (const s of stages) lines.push(`  - ${s}`);
  }
  const jobs = pipeline.definition_jobs ?? [];
  if (jobs.length > 0) {
    lines.push("jobs:");
    for (const j of jobs) {
      lines.push(`  - name: ${j.name}`);
      lines.push(`    stage: ${j.stage}`);
    }
  }
  return lines.join("\n");
}

function Kpi({
  label,
  value,
  hint,
  toneFrom,
}: {
  label: string;
  value: string;
  hint?: string;
  toneFrom?: number;
}) {
  const tone =
    toneFrom == null
      ? "text-foreground"
      : toneFrom >= 0.9
        ? "text-emerald-500"
        : toneFrom >= 0.7
          ? "text-amber-500"
          : "text-red-500";
  return (
    <div className="rounded-md border border-border bg-background px-3 py-2.5">
      <p className="text-[9px] font-semibold uppercase tracking-wider text-muted-foreground">
        {label}
      </p>
      <p
        className={cn("mt-1 font-mono text-lg font-semibold tabular-nums", tone)}
      >
        {value}
      </p>
      {hint ? (
        <p className="mt-0.5 text-[9px] text-muted-foreground">{hint}</p>
      ) : null}
    </div>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <section className="space-y-2">
      <h4 className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
        {title}
      </h4>
      {children}
    </section>
  );
}

function DefList({ children }: { children: React.ReactNode }) {
  return (
    <dl className="grid grid-cols-[84px_1fr] gap-x-3 gap-y-1 text-xs">
      {children}
    </dl>
  );
}

function Row({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <>
      <dt className="truncate text-muted-foreground">{label}</dt>
      <dd className="min-w-0 truncate">{children}</dd>
    </>
  );
}

function EdgeRow({
  pipelineName,
  hint,
  direction,
}: {
  pipelineName: string;
  hint: string;
  direction: "in" | "out";
}) {
  return (
    <li className="flex items-center gap-2 rounded-md border border-border bg-background px-2.5 py-1.5">
      {direction === "in" ? (
        <TrendingUp className="size-3 shrink-0 text-muted-foreground" aria-hidden />
      ) : (
        <ArrowRight className="size-3 shrink-0 text-muted-foreground" aria-hidden />
      )}
      <span className="min-w-0 flex-1 truncate font-mono text-xs">
        {pipelineName}
      </span>
      <span className="shrink-0 text-[10px] text-muted-foreground">{hint}</span>
    </li>
  );
}

function RecentRunRow({ run }: { run: RunSummary }) {
  return (
    <li>
      <Link
        href={`/runs/${run.id}` as Route}
        className="block rounded-md border border-border bg-background px-2.5 py-1.5 transition-colors hover:bg-accent"
      >
        <div className="flex items-center gap-2 text-xs">
          <StatusBadge status={run.status} className="text-[10px]" />
          <span className="font-mono text-[11px] text-muted-foreground">
            #{run.counter}
          </span>
          <span className="ml-auto text-[10px] text-muted-foreground">
            <RelativeTime at={run.started_at ?? run.created_at} />
          </span>
        </div>
        <div className="mt-0.5 flex items-center gap-2 text-[11px] text-muted-foreground">
          <span className="truncate">
            {run.triggered_by || run.cause || "—"}
          </span>
          <LiveDuration
            startedAt={run.started_at}
            finishedAt={run.finished_at}
            className="ml-auto shrink-0 font-mono tabular-nums"
          />
        </div>
      </Link>
    </li>
  );
}

function edgeHint(e: PipelineEdge): string {
  const bits: string[] = [];
  if (e.stage) bits.push(e.stage);
  if (e.status) bits.push(`on:${e.status}`);
  return bits.join(" · ");
}

function passRateTone(rate: number): string {
  if (rate >= 0.9) return "bg-emerald-500/10 text-emerald-500";
  if (rate >= 0.7) return "bg-amber-500/10 text-amber-500";
  return "bg-red-500/10 text-red-500";
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}
