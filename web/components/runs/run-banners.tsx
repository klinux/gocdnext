import Link from "next/link";
import type { Route } from "next";
import { GitPullRequest } from "lucide-react";

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
