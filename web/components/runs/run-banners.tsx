import type { Route } from "next";
import { GitPullRequest } from "lucide-react";

import { EntityChip } from "@/components/shared/entity-chip";

// Two small banners surfaced at the top of the run-detail page when
// the run was kicked off by something interesting — a PR or an
// upstream pipeline. Pure presentation, lifted out of run-live so
// the live client component stays focused on streaming + state.

export function PullRequestBanner({
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

export function UpstreamBanner({
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
  // Two chips share the banner: the pipeline that triggered this
  // run (no link target — pipelines are sub-views of their project,
  // not standalone) and the upstream run itself (clickable, takes
  // the operator to the parent run's logs). Stage name rides as a
  // hint inside the pipeline chip when present.
  return (
    <aside className="flex flex-wrap items-center gap-2 rounded-md border border-primary/30 bg-primary/5 px-3 py-2 text-sm">
      <span className="text-muted-foreground">Triggered by</span>
      {upstream_pipeline ? (
        <EntityChip
          kind="pipeline"
          label={upstream_pipeline}
          hint={upstream_stage ? `.${upstream_stage}` : undefined}
          direction="in"
        />
      ) : null}
      {upstream_run_id ? (
        <EntityChip
          kind="run"
          label={
            typeof upstream_run_counter === "number"
              ? `#${upstream_run_counter}`
              : "upstream run"
          }
          href={`/runs/${upstream_run_id}` as Route}
        />
      ) : null}
    </aside>
  );
}
