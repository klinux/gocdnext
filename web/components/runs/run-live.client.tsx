"use client";

import Link from "next/link";
import type { Route } from "next";
import { useMemo, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  ChevronRight,
  GitBranch,
  GitPullRequest,
  Radio,
} from "lucide-react";

import { cn } from "@/lib/utils";
import { Separator } from "@/components/ui/separator";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { LiveDuration } from "@/components/shared/live-duration";
import { StageSection } from "@/components/runs/stage-section";
import { RunArtifacts } from "@/components/runs/run-artifacts.client";
import { RunActions } from "@/components/runs/run-actions.client";
import { PipelineCanvas } from "@/components/runs/pipeline-canvas.client";
import { isTerminalStatus, statusTone, type StatusTone } from "@/lib/status";
import type { LogLine, RunDetail } from "@/types/api";

// LIVE_POLL_MS controls how fast the page requests the server for new log
// lines + status while the run is running. Small enough to feel live,
// big enough not to flood the API.
const LIVE_POLL_MS = 2_000;
const LOGS_PER_JOB = 500;

type Props = {
  initial: RunDetail;
  runId: string;
  apiBaseURL: string;
};

async function fetchRun(apiBaseURL: string, id: string): Promise<RunDetail> {
  const res = await fetch(
    `${apiBaseURL.replace(/\/+$/, "")}/api/v1/runs/${encodeURIComponent(id)}?logs=${LOGS_PER_JOB}`,
    // credentials: "include" forwards the session cookie cross-
    // origin (web dev on :3000 → control plane on :8153). The
    // control plane's devCORS echoes the Origin and sets
    // Access-Control-Allow-Credentials=true to let it through.
    { cache: "no-store", credentials: "include" },
  );
  if (!res.ok) throw new Error(`run fetch ${res.status}`);
  return (await res.json()) as RunDetail;
}

export function RunLive({ initial, runId, apiBaseURL }: Props) {
  const { data = initial } = useQuery({
    queryKey: ["run", runId],
    queryFn: () => fetchRun(apiBaseURL, runId),
    initialData: initial,
    refetchInterval: (query) => {
      const state = query.state.data?.status ?? initial.status;
      return isTerminalStatus(state) ? false : LIVE_POLL_MS;
    },
  });

  // The server returns a tail of LOGS_PER_JOB lines on every poll.
  // Without merging, a job producing >500 lines causes earlier lines
  // to slide off the tail and disappear from the UI between polls —
  // even though they're still in the DB. Keep a per-job seq→line
  // map across polls and render the merged history.
  const logsByJobRef = useRef<Map<string, Map<number, LogLine>>>(new Map());
  const mergedData = useMemo<RunDetail>(() => {
    const map = logsByJobRef.current;
    const stages = data.stages.map((stage) => ({
      ...stage,
      jobs: stage.jobs.map((job) => {
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
  }, [data]);

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

      <Separator />

      <div className="space-y-8">
        {mergedData.stages.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            This run has no stages.
          </p>
        ) : (
          mergedData.stages.map((s) => <StageSection key={s.id} stage={s} />)
        )}
      </div>

      <Separator />

      <section aria-label="Artifacts">
        <h3 className="mb-3 text-lg font-semibold tracking-tight">
          Artifacts
        </h3>
        <RunArtifacts
          runId={runId}
          runStatus={data.status}
          apiBaseURL={apiBaseURL}
        />
      </section>
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
  canceled: "bg-muted-foreground/60",
  skipped: "bg-muted-foreground/40",
  neutral: "bg-muted-foreground/40",
};

function PullRequestBanner({
  pr,
}: {
  pr: {
    pr_number?: number;
    pr_title?: string;
    pr_author?: string;
    pr_url?: string;
    pr_head_ref?: string;
    pr_head_sha?: string;
    pr_base_ref?: string;
  };
}) {
  return (
    <aside className="rounded-md border border-primary/30 bg-primary/5 px-3 py-2 text-sm">
      <div className="flex items-center gap-2">
        <GitPullRequest className="h-4 w-4 text-primary" aria-hidden />
        {pr.pr_url ? (
          <a
            href={pr.pr_url}
            target="_blank"
            rel="noreferrer noopener"
            className="font-mono text-primary hover:underline"
          >
            #{pr.pr_number}
          </a>
        ) : (
          <span className="font-mono">#{pr.pr_number}</span>
        )}
        {pr.pr_title ? <span className="truncate">{pr.pr_title}</span> : null}
      </div>
      <div className="mt-1 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {pr.pr_author ? (
          <span>
            by <span className="font-mono">@{pr.pr_author}</span>
          </span>
        ) : null}
        {pr.pr_head_ref && pr.pr_base_ref ? (
          <span>
            <span className="font-mono">{pr.pr_head_ref}</span> →{" "}
            <span className="font-mono">{pr.pr_base_ref}</span>
          </span>
        ) : null}
        {pr.pr_head_sha ? (
          <span className="font-mono">{pr.pr_head_sha.slice(0, 7)}</span>
        ) : null}
      </div>
    </aside>
  );
}

function UpstreamBanner({
  upstream,
}: {
  upstream: {
    upstream_run_id?: string;
    upstream_pipeline?: string;
    upstream_stage?: string;
    upstream_run_counter?: number;
  };
}) {
  const {
    upstream_run_id,
    upstream_pipeline,
    upstream_stage,
    upstream_run_counter,
  } = upstream;
  return (
    <aside className="rounded-md border border-primary/30 bg-primary/5 px-3 py-2 text-sm">
      Triggered by upstream{" "}
      <span className="font-mono">{upstream_pipeline}</span>
      {typeof upstream_run_counter === "number" ? (
        <span className="font-mono"> #{upstream_run_counter}</span>
      ) : null}
      {upstream_stage ? (
        <>
          {" "}after stage <span className="font-mono">{upstream_stage}</span>{" "}
          passed
        </>
      ) : null}
      {upstream_run_id ? (
        <>
          {" · "}
          <Link
            href={`/runs/${upstream_run_id}` as Route}
            className="text-primary hover:underline"
          >
            view upstream run
          </Link>
        </>
      ) : null}
    </aside>
  );
}
