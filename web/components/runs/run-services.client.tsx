"use client";

import { useQuery } from "@tanstack/react-query";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { StatusPill } from "@/components/shared/status-pill";
import { RelativeTime } from "@/components/shared/relative-time";
import { isTerminalStatus } from "@/lib/status";
import type { StatusTone } from "@/lib/status";
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

// PipelineCanvas (always mounted on the run-detail page) owns the
// polling for this query key. This tab just subscribes to the
// shared cache so opening it doesn't kick off a second interval
// chasing the same endpoint.
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
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>Image</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>Started</TableHead>
          <TableHead>Ready</TableHead>
          <TableHead>Duration</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((svc) => (
          <TableRow key={svc.id}>
            <TableCell className="font-mono text-xs">{svc.name}</TableCell>
            <TableCell className="font-mono text-xs">{svc.image}</TableCell>
            <TableCell>
              <StatusPill tone={statusTone(svc.status)}>
                {svc.status}
              </StatusPill>
              {svc.error ? (
                <p
                  title={svc.error}
                  className="mt-1 max-w-xs truncate text-xs text-destructive"
                >
                  {svc.error}
                </p>
              ) : null}
            </TableCell>
            <TableCell>
              {svc.started_at ? (
                <RelativeTime at={svc.started_at} />
              ) : (
                <span className="text-muted-foreground">—</span>
              )}
            </TableCell>
            <TableCell>
              {svc.ready_at ? (
                <RelativeTime at={svc.ready_at} />
              ) : (
                <span className="text-muted-foreground">—</span>
              )}
            </TableCell>
            <TableCell className="font-mono tabular-nums text-xs">
              {formatDuration(svc)}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function statusTone(status: RunService["status"]): StatusTone {
  switch (status) {
    case "ready":
      return "success";
    case "starting":
      return "running";
    case "stopped":
      return "neutral";
    case "failed":
      return "failed";
    default:
      return "neutral";
  }
}

// formatDuration shows the readiness window (started → ready)
// while a service is live, OR total uptime (started → stopped)
// once the run cleaned up. Missing started_at falls through to
// a `—` so the row stays renderable for events that arrived in
// an unusual order (e.g. `ready` before `starting` after a
// stream reconnect — the upsert preserves the first-observed
// timestamp on each column).
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
