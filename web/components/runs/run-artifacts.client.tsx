"use client";

import { useQuery } from "@tanstack/react-query";
import { Download, Package } from "lucide-react";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusPill } from "@/components/shared/status-pill";
import { formatBytes } from "@/lib/format";
import { isTerminalStatus } from "@/lib/status";
import type { StatusTone } from "@/lib/status";
import type { RunArtifact } from "@/types/api";

const POLL_MS = 5_000;

type Props = {
  runId: string;
  runStatus: string;
  apiBaseURL: string;
};

async function fetchArtifacts(
  apiBaseURL: string,
  id: string,
): Promise<RunArtifact[]> {
  const res = await fetch(
    `${apiBaseURL.replace(/\/+$/, "")}/api/v1/runs/${encodeURIComponent(id)}/artifacts`,
    { cache: "no-store", credentials: "include" },
  );
  // 503 = backend not configured; treat as "no artefacts" so the section
  // just stays empty instead of breaking the page.
  if (res.status === 503) return [];
  if (!res.ok) throw new Error(`artifacts fetch ${res.status}`);
  return (await res.json()) as RunArtifact[];
}

export function RunArtifacts({ runId, runStatus, apiBaseURL }: Props) {
  const { data, isLoading, isError } = useQuery({
    queryKey: ["run-artifacts", runId],
    queryFn: () => fetchArtifacts(apiBaseURL, runId),
    // Poll while the run might still produce artefacts; stop once
    // terminal. Re-fetch every time the tab is focused so a stale
    // signed URL gets refreshed.
    refetchInterval: isTerminalStatus(runStatus) ? false : POLL_MS,
    refetchOnWindowFocus: true,
    // Short staleness so the signed URL (5min TTL) doesn't go cold.
    staleTime: 60_000,
  });

  if (isLoading) {
    return <EmptyNote>Loading artifacts…</EmptyNote>;
  }
  if (isError) {
    return <EmptyNote>Couldn't load artifacts.</EmptyNote>;
  }
  const rows = data ?? [];
  if (rows.length === 0) {
    return <EmptyNote>No artifacts uploaded for this run.</EmptyNote>;
  }

  const byJob = groupByJob(rows);

  return (
    <div className="space-y-6">
      {byJob.map(([jobName, items]) => (
        <div key={jobName}>
          <h4 className="mb-2 flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <Package className="h-3.5 w-3.5" aria-hidden />
            <span className="font-mono">{jobName}</span>
            <span className="text-xs">
              · {items.length} artifact{items.length === 1 ? "" : "s"}
            </span>
          </h4>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Path</TableHead>
                <TableHead className="w-28">Size</TableHead>
                <TableHead className="w-24">Status</TableHead>
                <TableHead className="w-40">Created</TableHead>
                <TableHead className="w-44">SHA-256</TableHead>
                <TableHead className="w-28 text-right">Download</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((a) => (
                <TableRow key={a.id} className="font-mono text-xs">
                  <TableCell className="truncate">{a.path}</TableCell>
                  <TableCell>{formatBytes(a.size_bytes)}</TableCell>
                  <TableCell>
                    <ArtifactStatusPill status={a.status} />
                  </TableCell>
                  <TableCell>
                    <RelativeTime at={a.created_at} />
                  </TableCell>
                  <TableCell
                    className="truncate text-muted-foreground"
                    title={a.content_sha256}
                  >
                    {a.content_sha256 ? a.content_sha256.slice(0, 12) : "—"}
                  </TableCell>
                  <TableCell className="text-right">
                    {a.download_url ? (
                      <a
                        href={a.download_url}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="inline-flex items-center gap-1 text-primary hover:underline"
                      >
                        <Download className="h-3.5 w-3.5" aria-hidden />
                        download
                      </a>
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ))}
    </div>
  );
}

function EmptyNote({ children }: { children: React.ReactNode }) {
  return (
    <p className="rounded-md border border-dashed border-border px-4 py-6 text-center text-sm text-muted-foreground">
      {children}
    </p>
  );
}

function ArtifactStatusPill({ status }: { status: RunArtifact["status"] }) {
  const tone: StatusTone =
    status === "ready" ? "success" : status === "pending" ? "warning" : "neutral";
  return (
    <StatusPill tone={tone} className="uppercase tracking-wide text-[10px]">
      {status}
    </StatusPill>
  );
}

function groupByJob(
  rows: RunArtifact[],
): Array<[string, RunArtifact[]]> {
  const map = new Map<string, RunArtifact[]>();
  for (const a of rows) {
    const list = map.get(a.job_name) ?? [];
    list.push(a);
    map.set(a.job_name, list);
  }
  return [...map.entries()].sort(([a], [b]) => a.localeCompare(b));
}

