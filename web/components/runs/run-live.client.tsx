"use client";

import Link from "next/link";
import type { Route } from "next";
import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, GitBranch, Radio } from "lucide-react";

import { cn } from "@/lib/utils";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { LiveDuration } from "@/components/shared/live-duration";
import { RunActions } from "@/components/runs/run-actions.client";
import { RunTabs } from "@/components/runs/run-tabs.client";
import { PipelineCanvas } from "@/components/runs/pipeline-canvas.client";
import { PullRequestBanner, UpstreamBanner } from "@/components/runs/run-banners";
import { isTerminalStatus, statusTone, type StatusTone } from "@/lib/status";
import type { LogLine, RunDetail } from "@/types/api";

// LIVE_POLL_MS controls how fast the page refetches run + stage + job
// STATUS while the run is running. Log lines now arrive via SSE so
// the poll itself doesn't need log payloads — we still request a
// small tail (SAFETY_LOGS_PER_JOB) as a gap-filler in case the SSE
// handshake dropped a line published between the SSR fetch and the
// stream's `: ready` comment. Small enough to feel instant on
// status flips, big enough not to flood the API.
const LIVE_POLL_MS = 2_000;
const SAFETY_LOGS_PER_JOB = 50;

type Props = {
  initial: RunDetail;
  runId: string;
  apiBaseURL: string;
};

async function fetchRun(
  apiBaseURL: string,
  id: string,
  cursors: Record<string, number>,
): Promise<RunDetail> {
  const base = apiBaseURL.replace(/\/+$/, "");
  const url = new URL(`${base}/api/v1/runs/${encodeURIComponent(id)}`);
  url.searchParams.set("logs", String(SAFETY_LOGS_PER_JOB));
  // Per-job cursors: `?since=<job_id>:<last_seen_seq>` (repeated).
  // When present, the server returns only lines with seq > cursor
  // for that job, which keeps the delta small AND makes bursty
  // jobs (go test -v, webpack builds) that produce >LOGS_PER_JOB
  // lines between polls no longer drop the middle chunk. Jobs
  // missing from the map fall back to the tail — that's what the
  // first fetch after mount gets.
  for (const [jobID, seq] of Object.entries(cursors)) {
    url.searchParams.append("since", `${jobID}:${seq}`);
  }
  const res = await fetch(url.toString(), {
    // credentials: "include" forwards the session cookie cross-
    // origin (web dev on :3000 → control plane on :8153). The
    // control plane's devCORS echoes the Origin and sets
    // Access-Control-Allow-Credentials=true to let it through.
    cache: "no-store",
    credentials: "include",
  });
  if (!res.ok) throw new Error(`run fetch ${res.status}`);
  return (await res.json()) as RunDetail;
}

export function RunLive({ initial, runId, apiBaseURL }: Props) {
  // The log state that survives across polls. Each bucket is a
  // seq→line map so deltas coming back from the server merge in
  // O(lines) via Map.set. A separate ref tracks the last seen
  // status so a rerun (terminal → queued/running, seq counter
  // resets to 0) clears the bucket before the new lines land —
  // otherwise the cursor from the old attempt would filter out
  // every line of the new one.
  const logsByJobRef = useRef<Map<string, Map<number, LogLine>>>(new Map());
  const prevStatusRef = useRef<Map<string, string>>(new Map());
  // sseRev bumps every time an SSE event lands a new line. It exists
  // only to retrigger the useMemo that flattens logsByJobRef back
  // into job.logs arrays — the ref itself doesn't trigger a React
  // re-render, so we need a discrete state handle.
  const [sseRev, setSseRev] = useState(0);

  const { data = initial } = useQuery({
    queryKey: ["run", runId],
    queryFn: () => {
      // Derive cursors from the in-flight merge state right before
      // firing the fetch so we pick up any lines the previous
      // poll already filed.
      const cursors: Record<string, number> = {};
      for (const [jobID, bucket] of logsByJobRef.current) {
        let maxSeq = -1;
        for (const seq of bucket.keys()) {
          if (seq > maxSeq) maxSeq = seq;
        }
        if (maxSeq >= 0) cursors[jobID] = maxSeq;
      }
      return fetchRun(apiBaseURL, runId, cursors);
    },
    initialData: initial,
    refetchInterval: (query) => {
      const state = query.state.data?.status ?? initial.status;
      return isTerminalStatus(state) ? false : LIVE_POLL_MS;
    },
  });

  // SSE live log stream. Opens while the run is not terminal; the
  // browser's EventSource auto-reconnects on disconnect with a
  // Last-Event-ID header matching the last `id:` we emitted, so a
  // transient proxy blip doesn't tear the stream. We feed each
  // event into logsByJobRef by (job_id, seq) — the existing merge
  // path handles dedupe with the polling delta.
  useEffect(() => {
    if (isTerminalStatus(data.status)) return;
    const base = apiBaseURL.replace(/\/+$/, "");
    const url = `${base}/api/v1/runs/${encodeURIComponent(runId)}/logs/stream`;
    const es = new EventSource(url, { withCredentials: true });
    es.addEventListener("log", (ev) => {
      try {
        const payload = JSON.parse((ev as MessageEvent).data) as {
          job_id: string;
          seq: number;
          stream: string;
          at: string;
          text: string;
        };
        let bucket = logsByJobRef.current.get(payload.job_id);
        if (!bucket) {
          bucket = new Map<number, LogLine>();
          logsByJobRef.current.set(payload.job_id, bucket);
        }
        bucket.set(payload.seq, {
          seq: payload.seq,
          stream: payload.stream,
          at: payload.at,
          text: payload.text,
        });
        setSseRev((r) => r + 1);
      } catch {
        // Malformed frame — drop. The poll path will pick the
        // line up on the next tick.
      }
    });
    return () => es.close();
  }, [apiBaseURL, runId, data.status]);

  const mergedData = useMemo<RunDetail>(() => {
    const map = logsByJobRef.current;
    const prevStatus = prevStatusRef.current;
    const stages = data.stages.map((stage) => ({
      ...stage,
      jobs: stage.jobs.map((job) => {
        // Rerun detection: prior status was terminal and the new
        // one is queued/running → wipe the bucket so the old
        // attempt's lines don't hang around AND the cursor resets.
        const prior = prevStatus.get(job.id);
        if (
          prior &&
          isTerminalStatus(prior) &&
          (job.status === "queued" || job.status === "running")
        ) {
          map.delete(job.id);
        }
        prevStatus.set(job.id, job.status);

        let bucket = map.get(job.id);
        if (!bucket) {
          bucket = new Map<number, LogLine>();
          map.set(job.id, bucket);
        }
        for (const line of job.logs ?? []) bucket.set(line.seq, line);
        const merged = Array.from(bucket.values()).sort(
          (a, b) => a.seq - b.seq,
        );
        return { ...job, logs: merged };
      }),
    }));
    return { ...data, stages };
    // sseRev deliberately listed so a stream-delivered line forces
    // the flatten pass to re-run — the ref mutation alone doesn't
    // trigger a React update.
  }, [data, sseRev]);

  const upstream =
    data.cause === "upstream" && data.cause_detail
      ? (data.cause_detail as {
          upstream_run_id?: string;
          upstream_pipeline?: string;
          upstream_stage?: string;
          upstream_run_counter?: number;
        })
      : null;

  const pullRequest =
    data.cause === "pull_request" && data.cause_detail
      ? (data.cause_detail as {
          pr_number?: number;
          pr_title?: string;
          pr_author?: string;
          pr_url?: string;
          pr_head_ref?: string;
          pr_head_sha?: string;
          pr_base_ref?: string;
        })
      : null;

  const live = !isTerminalStatus(data.status);
  const tone: StatusTone = statusTone(data.status);
  const primaryRevision = pickRevision(data.revisions);

  return (
    <section className="space-y-6">
      <header className="space-y-3">
        <nav aria-label="Breadcrumb" className="text-xs text-muted-foreground">
          <Link href="/projects" className="hover:text-foreground">
            Projects
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <Link
            href={`/projects/${data.project_slug}` as Route}
            className="hover:text-foreground"
          >
            {data.project_slug}
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <span className="font-mono">
            {data.pipeline_name} #{data.counter}
          </span>
        </nav>

        <div className="flex flex-wrap items-center gap-2">
          <span
            className={cn(
              "inline-flex size-3 shrink-0 items-center justify-center rounded-full",
              toneDotClasses[tone],
              data.status === "running" && "animate-pulse",
            )}
            aria-hidden
            title={data.status}
          />
          <h2 className="font-mono text-xl font-semibold tracking-tight">
            {data.pipeline_name}
          </h2>
          <Link
            href={`/runs/${runId}` as Route}
            className="font-mono text-sm text-muted-foreground hover:text-foreground"
          >
            #{data.counter}
          </Link>
          <StatusBadge status={data.status} className="text-[10px]" />
          {live ? (
            <span
              role="status"
              aria-live="polite"
              className="inline-flex items-center gap-1 rounded-md border border-primary/30 bg-primary/5 px-2 py-0.5 text-[10px] uppercase tracking-wide text-primary"
            >
              <Radio className="h-3 w-3 animate-pulse" aria-hidden />
              Live
            </span>
          ) : null}
          <div className="ml-auto">
            <RunActions runId={runId} status={data.status} />
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
          {primaryRevision?.branch ? (
            <span
              className="inline-flex items-center gap-1 rounded-md bg-muted px-1.5 py-0.5 font-mono text-[10px]"
              title={`Ref: ${primaryRevision.branch}`}
            >
              <GitBranch className="size-3" aria-hidden />
              <span className="max-w-[200px] truncate">
                {primaryRevision.branch}
              </span>
            </span>
          ) : null}
          {primaryRevision?.revision ? (
            <span
              className="rounded bg-muted/80 px-1.5 py-0.5 font-mono text-[10px] text-foreground/80"
              title={primaryRevision.revision}
            >
              {primaryRevision.revision.slice(0, 7)}
            </span>
          ) : null}
          <LiveDuration
            startedAt={data.started_at}
            finishedAt={data.finished_at}
            className="font-mono tabular-nums text-foreground"
          />
          <span>·</span>
          <RelativeTime at={data.started_at ?? data.created_at} fallback="—" />
          <span>·</span>
          <span>
            cause <span className="font-mono text-foreground">{data.cause}</span>
          </span>
          {data.triggered_by ? (
            <>
              <span>·</span>
              <span>
                by{" "}
                <span className="font-mono text-foreground">
                  {data.triggered_by}
                </span>
              </span>
            </>
          ) : null}
        </div>
      </header>

      {upstream ? <UpstreamBanner upstream={upstream} /> : null}
      {pullRequest ? <PullRequestBanner pr={pullRequest} /> : null}

      <PipelineCanvas stages={mergedData.stages} />

      <RunTabs runId={runId} run={mergedData} apiBaseURL={apiBaseURL} />
    </section>
  );
}

// pickRevision chooses a representative (branch, revision) from
// the run's revisions JSONB. Multiple materials are possible but
// the header has space for only one — first deterministic entry
// wins (map key order via Object.keys is stable for string keys
// in ES2015+ and identical across server/client, which matters
// for hydration).
function pickRevision(
  revisions: RunDetail["revisions"],
): { revision: string; branch: string } | null {
  if (!revisions) return null;
  for (const key of Object.keys(revisions)) {
    const entry = revisions[key];
    if (entry && (entry.revision || entry.branch)) return entry;
  }
  return null;
}

const toneDotClasses: Record<StatusTone, string> = {
  success: "bg-emerald-500",
  failed: "bg-red-500",
  running: "bg-sky-500",
  queued: "bg-amber-500",
  warning: "bg-amber-500",
  awaiting: "bg-amber-500",
  canceled: "bg-muted-foreground/60",
  skipped: "bg-muted-foreground/40",
  neutral: "bg-muted-foreground/40",
};

