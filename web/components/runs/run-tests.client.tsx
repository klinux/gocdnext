"use client";

import Link from "next/link";
import type { Route } from "next";
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  CircleAlert,
  History,
  Loader2,
  MinusCircle,
  TestTube,
  XCircle,
} from "lucide-react";

import { cn } from "@/lib/utils";
import { isTerminalStatus } from "@/lib/status";
import type {
  RunDetail,
  TestCase,
  TestCaseHistoryEntry,
  TestCaseHistoryResponse,
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

// Exported so the tab strip on RunLive can prefetch with the same
// queryKey ["run-tests", runId] and react-query's cache dedupes the
// network call — no double fetch when the tab body mounts.
export async function fetchTests(
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
          apiBaseURL={apiBaseURL}
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
  apiBaseURL,
}: {
  summary: TestSummary;
  jobLabel: string;
  cases: TestCase[];
  apiBaseURL: string;
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
            <CaseRow key={c.id} c={c} apiBaseURL={apiBaseURL} />
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

function CaseRow({ c, apiBaseURL }: { c: TestCase; apiBaseURL: string }) {
  const [open, setOpen] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  const canExpand =
    (c.failure_message ?? "").length > 0 || (c.failure_detail ?? "").length > 0;

  return (
    <div className="px-4 py-2.5">
      <div className="flex w-full items-center gap-3">
        <button
          type="button"
          onClick={() => canExpand && setOpen((o) => !o)}
          className={cn(
            "flex min-w-0 flex-1 items-center gap-3 text-left",
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
        <button
          type="button"
          onClick={() => setHistoryOpen((o) => !o)}
          aria-label={`Show history for ${c.name}`}
          aria-expanded={historyOpen}
          className={cn(
            "shrink-0 rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground",
            historyOpen && "bg-muted text-foreground",
          )}
          title="Flakiness history"
        >
          <History className="size-3.5" aria-hidden />
        </button>
      </div>

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

      {historyOpen ? (
        <CaseHistory
          classname={c.classname ?? ""}
          name={c.name}
          apiBaseURL={apiBaseURL}
        />
      ) : null}
    </div>
  );
}

function CaseHistory({
  classname,
  name,
  apiBaseURL,
}: {
  classname: string;
  name: string;
  apiBaseURL: string;
}) {
  const { data, isLoading, isError } = useQuery({
    queryKey: ["test-history", classname, name],
    queryFn: () => fetchCaseHistory(apiBaseURL, classname, name),
    // Once loaded, keep for the session — flake history doesn't
    // change while the tab is open (new runs wouldn't be visible
    // without a refetch anyway; users reopen the popover to see
    // the newest entry).
    staleTime: 60_000,
    refetchOnWindowFocus: false,
  });

  return (
    <div className="mt-2 rounded-md border border-border bg-muted/20 p-3">
      <div className="mb-2 flex items-center gap-1.5 text-xs font-semibold text-muted-foreground">
        <History className="size-3.5" aria-hidden />
        Last executions
      </div>

      {isLoading ? (
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" aria-hidden />
          Loading history…
        </div>
      ) : isError ? (
        <p className="text-xs text-muted-foreground">Couldn&apos;t load history.</p>
      ) : (data?.entries.length ?? 0) === 0 ? (
        <p className="text-xs text-muted-foreground">
          No prior executions recorded.
        </p>
      ) : (
        <>
          <HistoryStrip entries={data!.entries} />
          <HistoryList entries={data!.entries} />
        </>
      )}
    </div>
  );
}

// HistoryStrip shows a row of colored squares — one per run —
// newest on the LEFT (matches the reversed chronological order
// the API returns). Hovering a square reveals the per-run
// status + counter via the native title tooltip so the mental
// model is "rightmost is oldest, scan-left to see recency".
function HistoryStrip({ entries }: { entries: TestCaseHistoryEntry[] }) {
  // Flakiness score: number of status transitions across the
  // visible window. Zero transitions = stable (all green or
  // all red). One or more transitions = has flaked at least once.
  let transitions = 0;
  for (let i = 1; i < entries.length; i++) {
    if (entries[i]!.status !== entries[i - 1]!.status) transitions++;
  }
  const flaky = transitions > 0;

  return (
    <div className="mb-3 flex items-center gap-2">
      <div className="flex items-center gap-0.5">
        {entries.map((e) => (
          <span
            key={e.id}
            title={`${e.pipeline_name} #${e.run_counter} · ${e.status}`}
            className={cn(
              "size-3 rounded-sm border",
              dotClassForStatus(e.status),
            )}
            aria-hidden
          />
        ))}
      </div>
      <span
        className={cn(
          "rounded-md border px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wider",
          flaky
            ? "border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-400"
            : entries[0]?.status === "passed"
              ? "border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400"
              : "border-red-500/40 bg-red-500/10 text-red-700 dark:text-red-400",
        )}
      >
        {flaky ? `flaky · ${transitions} flip${transitions === 1 ? "" : "s"}` : "stable"}
      </span>
    </div>
  );
}

function HistoryList({ entries }: { entries: TestCaseHistoryEntry[] }) {
  return (
    <ul className="space-y-1">
      {entries.map((e) => (
        <li
          key={e.id}
          className="flex items-center gap-3 text-xs text-muted-foreground"
        >
          <StatusGlyph status={e.status} />
          <Link
            href={`/runs/${e.run_id}` as Route}
            className="font-mono text-foreground hover:underline"
          >
            {e.pipeline_name} #{e.run_counter}
          </Link>
          <span className="ml-auto font-mono tabular-nums">
            {formatDuration(e.duration_ms)}
          </span>
          <time dateTime={e.at} className="tabular-nums">
            {formatRelativeShort(e.at)}
          </time>
        </li>
      ))}
    </ul>
  );
}

async function fetchCaseHistory(
  apiBaseURL: string,
  classname: string,
  name: string,
): Promise<TestCaseHistoryResponse> {
  const base = apiBaseURL.replace(/\/+$/, "");
  const url = new URL(`${base}/api/v1/tests/history`);
  if (classname) url.searchParams.set("classname", classname);
  url.searchParams.set("name", name);
  const res = await fetch(url.toString(), {
    cache: "no-store",
    credentials: "include",
  });
  if (!res.ok) throw new Error(`history fetch ${res.status}`);
  return (await res.json()) as TestCaseHistoryResponse;
}

function dotClassForStatus(status: string): string {
  switch (status) {
    case "passed":
      return "bg-emerald-500 border-emerald-600";
    case "failed":
      return "bg-red-500 border-red-600";
    case "errored":
      return "bg-red-500 border-red-600";
    case "skipped":
      return "bg-muted-foreground/40 border-muted-foreground/60";
    default:
      return "bg-muted-foreground/30 border-muted-foreground/40";
  }
}

// formatRelativeShort is a minimal "how long ago" stamp for the
// history list. We import the app's RelativeTime elsewhere but
// it's a full component with live updates — overkill for a
// static history entry. This one-shot format keeps the popover
// cheap.
function formatRelativeShort(iso: string): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "—";
  const diff = (Date.now() - then) / 1000;
  if (diff < 60) return `${Math.floor(diff)}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
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
