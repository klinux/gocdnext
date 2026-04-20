"use client";

import Link from "next/link";
import type { Route } from "next";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, GitPullRequest, Radio } from "lucide-react";
import { Separator } from "@/components/ui/separator";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { StageSection } from "@/components/runs/stage-section";
import { RunArtifacts } from "@/components/runs/run-artifacts.client";
import { RunActions } from "@/components/runs/run-actions.client";
import { durationBetween, formatDurationSeconds } from "@/lib/format";
import { isTerminalStatus } from "@/lib/status";
import type { RunDetail } from "@/types/api";

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

  const totalDuration = formatDurationSeconds(
    durationBetween(data.started_at, data.finished_at),
  );

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

  return (
    <section className="space-y-6">
      <header>
        <nav aria-label="Breadcrumb" className="text-xs text-muted-foreground">
          <Link href="/" className="hover:text-foreground">
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

        <div className="mt-2 flex flex-wrap items-center gap-3">
          <h2 className="text-2xl font-semibold tracking-tight">
            {data.pipeline_name}{" "}
            <span className="font-mono text-muted-foreground">
              #{data.counter}
            </span>
          </h2>
          <StatusBadge status={data.status} />
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

        <dl className="mt-2 flex flex-wrap gap-x-6 gap-y-1 text-xs text-muted-foreground">
          <Meta k="cause" v={data.cause} />
          <Meta
            k="started"
            v={
              <RelativeTime
                at={data.started_at ?? data.created_at}
                fallback="—"
              />
            }
          />
          <Meta k="duration" v={totalDuration} />
          {data.triggered_by ? <Meta k="triggered by" v={data.triggered_by} /> : null}
        </dl>
      </header>

      {upstream ? <UpstreamBanner upstream={upstream} /> : null}
      {pullRequest ? <PullRequestBanner pr={pullRequest} /> : null}

      <Separator />

      <div className="space-y-8">
        {data.stages.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            This run has no stages.
          </p>
        ) : (
          data.stages.map((s) => <StageSection key={s.id} stage={s} />)
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

function Meta({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div>
      <dt className="inline text-[10px] uppercase tracking-wide text-muted-foreground/70">
        {k}
      </dt>{" "}
      <dd className="inline font-mono">{v}</dd>
    </div>
  );
}

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
