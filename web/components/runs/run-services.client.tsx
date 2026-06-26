"use client";

import { useQuery } from "@tanstack/react-query";
import { Database, Clock } from "lucide-react";

import { cn } from "@/lib/utils";
import { ServiceGlyph, detectTech } from "@/components/shared/service-tech";
import type { RunService } from "@/types/api";

type Props = {
  runId: string;
  runStatus: string;
  apiBaseURL: string;
};

// fetchServices is exported so the tab strip on RunLive can
// prefetch with the same queryKey ["run-services", runId] —
// react-query dedupes the network call when the tab body mounts.
export async function fetchServices(
  apiBaseURL: string,
  id: string,
): Promise<RunService[]> {
  const res = await fetch(
    `${apiBaseURL.replace(/\/+$/, "")}/api/v1/runs/${encodeURIComponent(id)}/services`,
    { cache: "no-store", credentials: "include" },
  );
  if (!res.ok) throw new Error(`services fetch ${res.status}`);
  return (await res.json()) as RunService[];
}

// RunServices renders the run's environment containers as the design's
// Services band: per-tech tinted cards with image:tag, a readiness badge
// (live status) and the pod. Services boot before the jobs and stay alive
// for the whole run — host:port and per-job "uses" are intentionally out
// (the model is pipeline-scoped; we show status + pod, the data we have).
//
// PipelineCanvas (always mounted on the run-detail page) owns the polling
// for this query key; this tab just subscribes to the shared cache.
export function RunServices({ runId, runStatus: _runStatus, apiBaseURL }: Props) {
  const { data, isLoading, isError } = useQuery({
    queryKey: ["run-services", runId],
    queryFn: () => fetchServices(apiBaseURL, runId),
    refetchOnWindowFocus: true,
    staleTime: 30_000,
  });

  if (isLoading) return <EmptyState>Loading services…</EmptyState>;
  if (isError) return <EmptyState>Couldn’t load services.</EmptyState>;

  const rows = data ?? [];
  if (rows.length === 0) {
    return (
      <EmptyState>
        No services declared for this run. Add a <code>services:</code> block
        to the pipeline YAML to bring up databases or other sidecars.
      </EmptyState>
    );
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Database className="size-4" style={{ color: "#a779e9" }} aria-hidden />
        <span className="font-semibold">Services</span>
        <span className="text-muted-foreground">
          {rows.length} environment container{rows.length === 1 ? "" : "s"} ·
          start before the jobs
        </span>
        <span
          className="ml-auto inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium"
          style={{ borderColor: "rgba(167,121,233,.38)", color: "#b495e6" }}
        >
          <Clock className="size-3" aria-hidden />
          alive for the whole run
        </span>
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {rows.map((svc) => (
          <ServiceCard key={svc.id} svc={svc} />
        ))}
      </div>
    </div>
  );
}

function ServiceCard({ svc }: { svc: RunService }) {
  const tech = detectTech(svc.image || svc.name);
  return (
    <div className="rounded-xl border border-border bg-card p-3.5">
      <div className="flex items-center gap-3">
        <ServiceGlyph tech={tech} className="size-8 shrink-0" />
        <div className="min-w-0 flex-1">
          <div className="truncate font-mono text-sm font-semibold">{svc.name}</div>
          <div className="truncate font-mono text-[11px] text-muted-foreground">
            {svc.image}
          </div>
        </div>
        <ReadinessBadge status={svc.status} />
      </div>
      <div className="mt-3 flex items-center justify-between gap-2 border-t border-border/60 pt-2 text-[11px] text-muted-foreground">
        <span className="truncate font-mono" title={svc.pod_name ?? undefined}>
          {svc.pod_name ?? svc.name}
        </span>
        <span className="shrink-0 font-mono tabular-nums">{formatDuration(svc)}</span>
      </div>
      {svc.error ? (
        <p className="mt-1 truncate text-[11px] text-destructive" title={svc.error}>
          {svc.error}
        </p>
      ) : null}
    </div>
  );
}

const READINESS: Record<
  RunService["status"],
  { label: string; color: string; pulse: boolean }
> = {
  ready: { label: "READY", color: "#3fb950", pulse: true },
  starting: { label: "BOOTING", color: "#d9a429", pulse: false },
  failed: { label: "FAILED", color: "#f85149", pulse: false },
  stopped: { label: "STOPPED", color: "#6e7681", pulse: false },
};

function ReadinessBadge({ status }: { status: RunService["status"] }) {
  const m = READINESS[status] ?? READINESS.stopped;
  return (
    <span
      className="inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2 py-0.5 text-[10px] font-semibold"
      style={{ borderColor: `${m.color}55`, color: m.color }}
    >
      <span
        className={cn("size-[7px] rounded-full", m.pulse && "animate-pulse")}
        style={{ background: m.color }}
        aria-hidden
      />
      {m.label}
    </span>
  );
}

// formatDuration shows the readiness window (started → ready) while a
// service is live, OR total uptime (started → stopped) once the run
// cleaned up. Missing/odd timestamps fall through to `—`.
function formatDuration(svc: RunService): string {
  if (!svc.started_at) return "—";
  const start = new Date(svc.started_at).getTime();
  const end = svc.stopped_at
    ? new Date(svc.stopped_at).getTime()
    : svc.ready_at
      ? new Date(svc.ready_at).getTime()
      : NaN;
  if (Number.isNaN(end) || end < start) return "—";
  const ms = end - start;
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  const m = Math.floor(ms / 60_000);
  const s = Math.floor((ms % 60_000) / 1000);
  return `${m}m${s}s`;
}

function EmptyState({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
      {children}
    </div>
  );
}
