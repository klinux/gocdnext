"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import { GitBranch } from "lucide-react";

import { LiveDuration } from "@/components/shared/live-duration";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { TriggerPipelineButton } from "@/components/pipelines/trigger-pipeline-button.client";
import { PipelineStageRow } from "@/components/pipelines/pipeline-stage-row";
import { PipelineOverviewSheet } from "@/components/pipelines/pipeline-overview-sheet.client";
import {
  buildColumns,
  pickBottleneck,
} from "@/components/pipelines/pipeline-card-helpers";
import {
  PipelineBottleneckCallout,
  PipelineMetricsFooter,
} from "@/components/pipelines/pipeline-metrics-footer";
import type {
  PipelineEdge,
  PipelineSummary,
  RunSummary,
} from "@/types/api";

type Props = {
  projectSlug: string;
  pipeline: PipelineSummary;
  // Edges + runs flow through from PipelineFlow so the overview
  // sheet can render upstream/downstream + recent runs without
  // a second fetch — everything is already in ProjectDetail.
  edges: PipelineEdge[];
  runs: RunSummary[];
  // Registered by the PipelineFlow overlay so it can measure this
  // card's geometry and draw an SVG arrow to/from it.
  nodeRef?: (el: HTMLElement | null) => void;
};

// PipelineCard is the bordered container for one pipeline. Jobs are
// the prominent visual cards inside (see pipeline-stage-row); this
// wrapper gives the pipeline itself a card identity — border, bg,
// and a divider under the header so the stage flow reads as a
// separate region. Click the header to open the pipeline overview
// sheet (KPIs + details + triggered-by + recent runs).
export function PipelineCard({
  projectSlug,
  pipeline,
  edges,
  runs,
  nodeRef,
}: Props) {
  const run = pipeline.latest_run;
  const meta = pipeline.latest_run_meta;
  const columns = useMemo(() => buildColumns(pipeline), [pipeline]);
  const bottleneck = useMemo(() => pickBottleneck(columns), [columns]);
  const metrics = pipeline.metrics;
  const [overviewOpen, setOverviewOpen] = useState(false);

  const commitSubject = meta?.message ? firstLine(meta.message) : null;
  const shortSha = meta?.revision ? meta.revision.slice(0, 7) : null;

  return (
    <>
      <article
        ref={nodeRef}
        className="flex flex-col overflow-hidden rounded-lg border border-border bg-card shadow-sm"
      >
        <header className="flex items-center justify-between gap-2 border-b border-border px-3 py-2">
          <div className="flex min-w-0 flex-1 flex-col gap-0.5">
            <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5">
              <button
                type="button"
                onClick={() => setOverviewOpen(true)}
                title="Open pipeline overview"
                className="truncate rounded-sm font-mono text-sm font-semibold outline-none hover:underline focus-visible:ring-2 focus-visible:ring-ring"
              >
                {pipeline.name}
              </button>
              <span className="text-[10px] text-muted-foreground">
                v{pipeline.definition_version}
              </span>
              {run ? (
                <span className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-[11px] text-muted-foreground">
                  <Link
                    href={`/runs/${run.id}` as Route}
                    className="font-mono text-foreground hover:underline"
                  >
                    #{run.counter}
                  </Link>
                  <LiveDuration
                    startedAt={run.started_at}
                    finishedAt={run.finished_at}
                    className="font-mono tabular-nums"
                  />
                  <span>
                    · <RelativeTime at={run.started_at ?? run.created_at} />
                  </span>
                </span>
              ) : (
                <span className="text-[11px] italic text-muted-foreground">
                  Never run
                </span>
              )}
            </div>
            {commitSubject || shortSha ? (
              <div className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
                {shortSha ? (
                  <span className="font-mono text-[10px]">{shortSha}</span>
                ) : null}
                {commitSubject ? (
                  <span
                    className="truncate text-foreground/80"
                    title={meta?.message}
                  >
                    {commitSubject}
                  </span>
                ) : null}
              </div>
            ) : null}
          </div>
          <div className="flex shrink-0 items-center gap-1.5">
            {run ? (
              <StatusBadge status={run.status} className="text-[10px]" />
            ) : null}
            {meta?.branch ? (
              <span
                className="inline-flex max-w-[160px] items-center gap-1 rounded-full bg-muted px-2 py-0.5 font-mono text-[10px] text-muted-foreground"
                title={`Ref: ${meta.branch}`}
              >
                <GitBranch className="size-3" aria-hidden />
                <span className="truncate">{meta.branch}</span>
              </span>
            ) : null}
            <TriggerPipelineButton
              pipelineId={pipeline.id}
              pipelineName={pipeline.name}
              projectSlug={projectSlug}
            />
          </div>
        </header>

        {/* flex-1 wrapper so the metrics footer sticks to the bottom
            when the grid row stretches this card to match a taller
            neighbour (e.g. a pipeline with multi-row stages). */}
        <div className="flex flex-1 flex-col">
          <PipelineStageRow columns={columns} runId={run?.id} />
          {bottleneck ? (
            <PipelineBottleneckCallout bottleneck={bottleneck} />
          ) : null}
        </div>

        {metrics ? <PipelineMetricsFooter metrics={metrics} /> : null}
      </article>

      <PipelineOverviewSheet
        open={overviewOpen}
        onOpenChange={setOverviewOpen}
        pipeline={pipeline}
        edges={edges}
        runs={runs}
      />
    </>
  );
}

// firstLine returns the commit message's subject line only —
// anything after the first newline is body that doesn't fit on
// one visual row. Falls through to the original when it's already
// short enough.
function firstLine(message: string): string {
  const idx = message.indexOf("\n");
  return idx >= 0 ? message.slice(0, idx) : message;
}
