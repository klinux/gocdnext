"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  CircleAlert,
  MinusCircle,
  TestTube,
  XCircle,
} from "lucide-react";

import { cn } from "@/lib/utils";
import { isTerminalStatus } from "@/lib/status";
import type {
  RunDetail,
  TestCase,
  TestResultsResponse,
  TestSummary,
} from "@/types/api";

const POLL_MS = 5_000;

type Props = {
  runId: string;
  runStatus: string;
  run: RunDetail;
  apiBaseURL: string;
};

async function fetchTests(
  apiBaseURL: string,
  id: string,
): Promise<TestResultsResponse> {
  const res = await fetch(
    `${apiBaseURL.replace(/\/+$/, "")}/api/v1/runs/${encodeURIComponent(id)}/tests`,
    { cache: "no-store", credentials: "include" },
  );
  if (!res.ok) throw new Error(`tests fetch ${res.status}`);
  return (await res.json()) as TestResultsResponse;
}

// RunTests renders a Tests tab on the run detail page: one card
// per job that reported, with pass/fail/skip counts + a
// collapsible list of failing cases. Polls while the run is
// still alive so a late-arriving TestResultBatch surfaces
// without a manual reload. Jobs that never produced reports are
// silently skipped — no "0/0" cards cluttering the tab.
export function RunTests({ runId, runStatus, run, apiBaseURL }: Props) {
  const { data, isLoading, isError } = useQuery({
    queryKey: ["run-tests", runId],
    queryFn: () => fetchTests(apiBaseURL, runId),
    refetchInterval: isTerminalStatus(runStatus) ? false : POLL_MS,
    refetchOnWindowFocus: true,
    staleTime: 30_000,
  });

  if (isLoading) {
    return <EmptyNote>Loading test results…</EmptyNote>;
  }
  if (isError) {
    return <EmptyNote>Couldn&apos;t load test results.</EmptyNote>;
  }
  const summaries = data?.summaries ?? [];
  const cases = data?.cases ?? [];
  if (summaries.length === 0) {
    return (
      <EmptyNote>
        No test reports uploaded for this run. Declare{" "}
        <code className="rounded bg-muted px-1 py-0.5 text-xs">test_reports:</code>{" "}
        on a job and ship a JUnit XML to surface per-case
        results here.
      </EmptyNote>
    );
  }

  const jobLabel = buildJobLabelMap(run);
  const casesByJob = groupCasesByJob(cases);

  return (
    <div className="space-y-4">
      <TotalsRow summaries={summaries} />
      {summaries.map((s) => (
        <JobCard
          key={s.job_run_id}
          summary={s}
          jobLabel={jobLabel[s.job_run_id] ?? s.job_run_id.slice(0, 8)}
          cases={casesByJob[s.job_run_id] ?? []}
        />
      ))}
    </div>
  );
}

function TotalsRow({ summaries }: { summaries: TestSummary[] }) {
  const totals = summaries.reduce(
    (acc, s) => ({
      total: acc.total + s.total,
      passed: acc.passed + s.passed,
      failed: acc.failed + s.failed,
      skipped: acc.skipped + s.skipped,
      errored: acc.errored + s.errored,
      duration_ms: acc.duration_ms + s.duration_ms,
    }),
    { total: 0, passed: 0, failed: 0, skipped: 0, errored: 0, duration_ms: 0 },
  );

  return (
    <div className="flex flex-wrap items-center gap-3 rounded-lg border border-border bg-card px-4 py-3">
      <span className="flex items-center gap-1.5 text-sm font-semibold">
        <TestTube className="size-4 text-muted-foreground" aria-hidden />
        {totals.total} test{totals.total === 1 ? "" : "s"}
      </span>
      <span className="text-muted-foreground">·</span>
      <Tally kind="passed" count={totals.passed} />
      {totals.failed > 0 ? <Tally kind="failed" count={totals.failed} /> : null}
      {totals.errored > 0 ? <Tally kind="errored" count={totals.errored} /> : null}
      {totals.skipped > 0 ? <Tally kind="skipped" count={totals.skipped} /> : null}
      <span className="ml-auto text-xs text-muted-foreground">
        {formatDuration(totals.duration_ms)}
      </span>
    </div>
  );
}

function JobCard({
  summary,
  jobLabel,
  cases,
}: {
  summary: TestSummary;
  jobLabel: string;
  cases: TestCase[];
}) {
  // Failing + errored cases open by default — that's why someone
  // is reading this tab. Passed/skipped cases collapsed behind a
  // "show all" toggle so a 10k-case suite doesn't render a
  // 10k-row list on page load.
  const [showAll, setShowAll] = useState(false);

  const failing = cases.filter(
    (c) => c.status === "failed" || c.status === "errored",
  );
  const rest = cases.filter(
    (c) => c.status !== "failed" && c.status !== "errored",
  );
  const visible = showAll ? [...failing, ...rest] : failing;

  return (
    <section className="rounded-lg border border-border bg-card">
      <header className="flex flex-wrap items-center gap-3 border-b border-border px-4 py-2.5">
        <span className="font-mono text-sm font-semibold">{jobLabel}</span>
        <div className="flex items-center gap-2">
          <Tally kind="passed" count={summary.passed} />
          {summary.failed > 0 ? <Tally kind="failed" count={summary.failed} /> : null}
          {summary.errored > 0 ? <Tally kind="errored" count={summary.errored} /> : null}
          {summary.skipped > 0 ? <Tally kind="skipped" count={summary.skipped} /> : null}
        </div>
        <span className="ml-auto text-xs text-muted-foreground">
          {formatDuration(summary.duration_ms)}
        </span>
      </header>

      {cases.length === 0 ? (
        <div className="px-4 py-3 text-sm text-muted-foreground">
          Counts only — the full case list didn&apos;t ship.
        </div>
      ) : (
        <div className="divide-y divide-border/50">
          {visible.map((c) => (
            <CaseRow key={c.id} c={c} />
          ))}
          {rest.length > 0 && !showAll ? (
            <button
              type="button"
              onClick={() => setShowAll(true)}
              className="flex w-full items-center justify-center gap-1.5 px-4 py-2 text-xs text-muted-foreground transition-colors hover:bg-muted/30 hover:text-foreground"
            >
              <ChevronDown className="size-3.5" aria-hidden />
              Show all {cases.length} cases
            </button>
          ) : null}
        </div>
      )}
    </section>
  );
}

function CaseRow({ c }: { c: TestCase }) {
  const [open, setOpen] = useState(false);
  const canExpand =
    (c.failure_message ?? "").length > 0 || (c.failure_detail ?? "").length > 0;

  return (
    <div className="px-4 py-2.5">
      <button
        type="button"
        onClick={() => canExpand && setOpen((o) => !o)}
        className={cn(
          "flex w-full items-center gap-3 text-left",
          canExpand && "cursor-pointer",
        )}
        aria-expanded={open}
      >
        <StatusGlyph status={c.status} />
        <div className="min-w-0 flex-1">
          <span className="font-mono text-sm">{c.name}</span>
          {c.classname ? (
            <span className="ml-2 font-mono text-[11px] text-muted-foreground">
              {c.classname}
            </span>
          ) : null}
        </div>
        <span className="font-mono text-[11px] text-muted-foreground">
          {formatDuration(c.duration_ms)}
        </span>
        {canExpand ? (
          <ChevronRight
            className={cn(
              "size-3.5 shrink-0 text-muted-foreground transition-transform",
              open && "rotate-90",
            )}
            aria-hidden
          />
        ) : null}
      </button>

      {open && canExpand ? (
        <div className="mt-2 space-y-2 rounded-md border border-border bg-muted/30 p-3">
          {c.failure_type ? (
            <div className="text-xs font-semibold text-destructive">
              {c.failure_type}
            </div>
          ) : null}
          {c.failure_message ? (
            <pre className="whitespace-pre-wrap break-words font-mono text-xs text-foreground">
              {c.failure_message}
            </pre>
          ) : null}
          {c.failure_detail ? (
            <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-words font-mono text-[11px] text-muted-foreground">
              {c.failure_detail}
            </pre>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

function Tally({
  kind,
  count,
}: {
  kind: "passed" | "failed" | "skipped" | "errored";
  count: number;
}) {
  const tones: Record<string, string> = {
    passed: "text-emerald-600 dark:text-emerald-400",
    failed: "text-red-600 dark:text-red-400",
    errored: "text-red-600 dark:text-red-400",
    skipped: "text-muted-foreground",
  };
  return (
    <span className={cn("inline-flex items-center gap-1 text-xs", tones[kind])}>
      <StatusGlyph status={kind} />
      <span className="font-mono tabular-nums">{count}</span>
    </span>
  );
}

function StatusGlyph({ status }: { status: string }) {
  const cls = "size-3.5";
  switch (status) {
    case "passed":
      return <CheckCircle2 className={cn(cls, "text-emerald-500")} aria-hidden />;
    case "failed":
      return <XCircle className={cn(cls, "text-red-500")} aria-hidden />;
    case "errored":
      return <CircleAlert className={cn(cls, "text-red-500")} aria-hidden />;
    case "skipped":
      return <MinusCircle className={cn(cls, "text-muted-foreground")} aria-hidden />;
    default:
      return <MinusCircle className={cn(cls, "text-muted-foreground")} aria-hidden />;
  }
}

function EmptyNote({ children }: { children: React.ReactNode }) {
  return (
    <p className="rounded-lg border border-dashed border-border p-6 text-center text-sm text-muted-foreground">
      {children}
    </p>
  );
}

function buildJobLabelMap(run: RunDetail): Record<string, string> {
  const out: Record<string, string> = {};
  for (const stage of run.stages) {
    for (const job of stage.jobs) {
      out[job.id] = job.matrix_key ? `${job.name} [${job.matrix_key}]` : job.name;
    }
  }
  return out;
}

function groupCasesByJob(cases: TestCase[]): Record<string, TestCase[]> {
  const out: Record<string, TestCase[]> = {};
  for (const c of cases) {
    (out[c.job_run_id] ??= []).push(c);
  }
  return out;
}

function formatDuration(ms: number): string {
  if (!ms) return "—";
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 2 : 1)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.floor(s - m * 60);
  return `${m}m ${rem}s`;
}
