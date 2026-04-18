"use client";

import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Radio } from "lucide-react";
import { Separator } from "@/components/ui/separator";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { StageSection } from "@/components/runs/stage-section";
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
    { cache: "no-store" },
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
            href={{
              pathname: "/projects/[slug]",
              query: { slug: data.project_slug },
            }}
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
            href={{
              pathname: "/runs/[id]",
              query: { id: upstream_run_id },
            }}
            className="text-primary hover:underline"
          >
            view upstream run
          </Link>
        </>
      ) : null}
    </aside>
  );
}
