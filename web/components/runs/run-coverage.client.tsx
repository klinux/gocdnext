"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { isTerminalStatus } from "@/lib/status";

const COVERAGE_POLL_MS = 5_000;

export type CoverageRow = {
  job_run_id: string;
  job_name: string;
  matrix_key?: string;
  format: string;
  lines_covered: number;
  lines_total: number;
  packages?: { name: string; lines_covered: number; lines_total: number }[];
  created_at: string;
};

export type CoverageTrendPoint = {
  run_id: string;
  job_name: string;
  matrix_key?: string;
  lines_covered: number;
  lines_total: number;
  created_at: string;
};

export async function fetchCoverage(
  apiBaseURL: string,
  runId: string,
): Promise<CoverageRow[]> {
  const res = await fetch(`${apiBaseURL}/api/v1/runs/${runId}/coverage`, {
    cache: "no-store",
  });
  if (!res.ok) throw new Error(`coverage: ${res.status}`);
  const body = (await res.json()) as { coverage: CoverageRow[] | null };
  return body.coverage ?? [];
}

async function fetchTrend(
  apiBaseURL: string,
  pipelineId: string,
): Promise<CoverageTrendPoint[]> {
  const res = await fetch(
    `${apiBaseURL}/api/v1/pipelines/${pipelineId}/coverage-trend?limit=30`,
    { cache: "no-store" },
  );
  if (!res.ok) throw new Error(`coverage trend: ${res.status}`);
  const body = (await res.json()) as { points: CoverageTrendPoint[] | null };
  return body.points ?? [];
}

// pct is THE percentage formula — one place, mirroring the agent's
// log line (100 * covered / total, one decimal). The UI must never
// disagree with what the job log printed.
export function pct(covered: number, total: number): string {
  if (total <= 0) return "—";
  return `${((100 * covered) / total).toFixed(1)}%`;
}

type Props = {
  runId: string;
  runStatus: string;
  pipelineId: string;
  apiBaseURL: string;
};

// RunCoverage renders one card per (job, matrix cell) summary. The
// trend sparkline is FILTERED per job name + matrix key — a
// pipeline's trend mixes every coverage-reporting job, and blending
// `unit` with `integration` into one line would chart noise.
export function RunCoverage({ runId, runStatus, pipelineId, apiBaseURL }: Props) {
  const { data, isLoading, error } = useQuery({
    queryKey: ["run-coverage", runId],
    queryFn: () => fetchCoverage(apiBaseURL, runId),
    refetchInterval: isTerminalStatus(runStatus) ? false : COVERAGE_POLL_MS,
    staleTime: 30_000,
  });
  const trendQuery = useQuery({
    queryKey: ["coverage-trend", pipelineId],
    queryFn: () => fetchTrend(apiBaseURL, pipelineId),
    staleTime: 60_000,
  });

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading coverage…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load coverage: {String(error)}
      </p>
    );
  }
  const rows = data ?? [];
  if (rows.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No coverage reported. Declare{" "}
        <code className="rounded bg-muted px-1 font-mono text-xs">
          coverage_report:
        </code>{" "}
        on a job (go-cover, lcov or cobertura) and its summary lands here.
      </p>
    );
  }

  return (
    <div className="space-y-4">
      {rows.map((row) => (
        <JobCoverageCard
          key={row.job_run_id}
          row={row}
          trend={(trendQuery.data ?? []).filter(
            (p) =>
              p.job_name === row.job_name &&
              (p.matrix_key ?? "") === (row.matrix_key ?? ""),
          )}
        />
      ))}
    </div>
  );
}

function JobCoverageCard({
  row,
  trend,
}: {
  row: CoverageRow;
  trend: CoverageTrendPoint[];
}) {
  const [open, setOpen] = useState(false);
  const title = row.matrix_key
    ? `${row.job_name} [${row.matrix_key}]`
    : row.job_name;
  const packages = row.packages ?? [];

  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between space-y-0">
        <div>
          <CardTitle className="font-mono text-sm">{title}</CardTitle>
          <CardDescription>
            {row.lines_covered.toLocaleString()} of{" "}
            {row.lines_total.toLocaleString()} lines · {row.format}
          </CardDescription>
        </div>
        <div className="flex items-center gap-4">
          <Sparkline points={trend} />
          <span className="text-2xl font-semibold tabular-nums">
            {pct(row.lines_covered, row.lines_total)}
          </span>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        <CoverageBar covered={row.lines_covered} total={row.lines_total} />
        {packages.length > 0 ? (
          <div>
            <button
              type="button"
              onClick={() => setOpen((v) => !v)}
              className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
            >
              {open ? (
                <ChevronDown className="size-3.5" aria-hidden />
              ) : (
                <ChevronRight className="size-3.5" aria-hidden />
              )}
              {packages.length} package(s) — worst coverage first
            </button>
            {open ? (
              <ul className="mt-2 space-y-1">
                {packages.map((p) => (
                  <li
                    key={p.name}
                    className="flex items-center justify-between gap-3 text-xs"
                  >
                    <code className="min-w-0 truncate font-mono">{p.name}</code>
                    <span className="flex shrink-0 items-center gap-2">
                      <span className="w-24">
                        <CoverageBar
                          covered={p.lines_covered}
                          total={p.lines_total}
                        />
                      </span>
                      <span className="w-14 text-right tabular-nums">
                        {pct(p.lines_covered, p.lines_total)}
                      </span>
                    </span>
                  </li>
                ))}
              </ul>
            ) : null}
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

function CoverageBar({ covered, total }: { covered: number; total: number }) {
  const ratio = total > 0 ? covered / total : 0;
  const tone =
    ratio >= 0.8 ? "bg-emerald-500" : ratio >= 0.5 ? "bg-amber-500" : "bg-red-500";
  return (
    <div
      className="h-1.5 w-full overflow-hidden rounded-full bg-muted"
      role="progressbar"
      aria-valuenow={Math.round(ratio * 100)}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      <div className={`h-full ${tone}`} style={{ width: `${ratio * 100}%` }} />
    </div>
  );
}

// Sparkline is a dependency-free inline SVG: ~30 points of pipeline
// history for THIS job. The endpoint returns newest-first; we flip
// so time flows left→right.
function Sparkline({ points }: { points: CoverageTrendPoint[] }) {
  if (points.length < 2) return null;
  const ordered = [...points].reverse();
  const w = 96;
  const h = 24;
  const ratios = ordered.map((p) =>
    p.lines_total > 0 ? p.lines_covered / p.lines_total : 0,
  );
  const min = Math.min(...ratios);
  const max = Math.max(...ratios);
  const span = max - min || 1;
  const coords = ratios
    .map((r, i) => {
      const x = (i / (ratios.length - 1)) * (w - 2) + 1;
      const y = h - 2 - ((r - min) / span) * (h - 4);
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
  return (
    <svg
      width={w}
      height={h}
      viewBox={`0 0 ${w} ${h}`}
      className="text-muted-foreground"
      aria-label={`coverage trend, ${points.length} runs`}
      role="img"
    >
      <polyline
        points={coords}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
      />
    </svg>
  );
}
