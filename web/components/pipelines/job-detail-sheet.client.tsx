"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import { ExternalLink, Loader2 } from "lucide-react";

import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";
import { StatusBadge } from "@/components/shared/status-badge";
import { LogViewer } from "@/components/runs/log-viewer";
import { RelativeTime } from "@/components/shared/relative-time";
import { LiveDuration } from "@/components/shared/live-duration";
import {
  fetchJobDetail,
  type JobDetailResult,
} from "@/server/actions/runs";

type Props = {
  runId: string;
  jobId: string;
  jobName: string;
  trigger: React.ReactElement;
};

// Drawer for a single job: timing, image, exit code, recent logs.
// Fetch fires lazily on open so closed triggers don't tax the API.
// The full run page is the canonical detail view; this drawer is the
// glanceable preview that keeps the user on the pipelines tab.
export function JobDetailSheet({ runId, jobId, jobName, trigger }: Props) {
  const [open, setOpen] = useState(false);
  const [result, setResult] = useState<JobDetailResult | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!open || result) return;
    setLoading(true);
    fetchJobDetail({ runId, jobId, logLines: 80 })
      .then(setResult)
      .finally(() => setLoading(false));
  }, [open, runId, jobId, result]);

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger render={trigger} />
      <SheetContent
        side="right"
        className="data-[side=right]:w-[560px] data-[side=right]:sm:max-w-[560px]"
      >
        <SheetHeader>
          <SheetTitle className="font-mono text-base">{jobName}</SheetTitle>
          <SheetDescription>Job summary and recent log tail.</SheetDescription>
        </SheetHeader>

        <div className="mt-4 space-y-5 px-4 pb-6">
          {loading && !result ? (
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" aria-hidden />
              Loading job details…
            </div>
          ) : null}

          {result && !result.ok ? (
            <p className="text-sm text-destructive">
              Couldn&apos;t load this job: {result.error}
            </p>
          ) : null}

          {result && result.ok ? (
            <JobDetailBody result={result} />
          ) : null}
        </div>
      </SheetContent>
    </Sheet>
  );
}

function JobDetailBody({ result }: { result: Extract<JobDetailResult, { ok: true }> }) {
  const { job, run, stageName } = result;

  return (
    <>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-3 text-sm">
        <Field label="Status">
          <StatusBadge status={job.status} />
        </Field>
        <Field label="Stage">
          <span className="font-mono text-xs">{stageName}</span>
        </Field>
        <Field label="Run">
          <Link
            href={`/runs/${run.id}` as Route}
            className="inline-flex items-center gap-1 font-mono text-xs hover:underline"
          >
            #{run.counter} · {run.pipeline_name}
            <ExternalLink className="size-3" aria-hidden />
          </Link>
        </Field>
        <Field label="Agent">
          <span className="font-mono text-xs">{job.agent_id ?? "—"}</span>
        </Field>
        <Field label="Image">
          <span className="truncate font-mono text-xs" title={job.image ?? ""}>
            {job.image ?? "—"}
          </span>
        </Field>
        <Field label="Matrix">
          <span className="font-mono text-xs">{job.matrix_key ?? "—"}</span>
        </Field>
        <Field label="Started">
          {job.started_at ? (
            <RelativeTime at={job.started_at} />
          ) : (
            <span className="text-muted-foreground">—</span>
          )}
        </Field>
        <Field label="Duration">
          {job.started_at ? (
            <LiveDuration
              startedAt={job.started_at}
              finishedAt={job.finished_at}
              className="font-mono text-xs"
            />
          ) : (
            <span className="font-mono text-xs">—</span>
          )}
        </Field>
        {typeof job.exit_code === "number" ? (
          <Field label="Exit code">
            <span
              className={
                job.exit_code === 0
                  ? "font-mono text-xs"
                  : "font-mono text-xs text-destructive"
              }
            >
              {job.exit_code}
            </span>
          </Field>
        ) : null}
      </dl>

      {job.error ? (
        <section>
          <h4 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Error
          </h4>
          <pre className="mt-2 overflow-auto rounded-md bg-destructive/10 p-3 font-mono text-xs text-destructive">
            {job.error}
          </pre>
        </section>
      ) : null}

      <section>
        <div className="mb-2 flex items-center justify-between">
          <h4 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Recent logs
          </h4>
          <Link
            href={`/runs/${run.id}#job-${job.id}` as Route}
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            Full run <ExternalLink className="size-3" aria-hidden />
          </Link>
        </div>
        <div className="rounded-md border border-border">
          <LogViewer logs={job.logs ?? []} />
        </div>
      </section>
    </>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
        {label}
      </dt>
      <dd className="mt-0.5 min-w-0 truncate">{children}</dd>
    </div>
  );
}
