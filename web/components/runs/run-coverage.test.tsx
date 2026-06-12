import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { RunCoverage, deltaPP, pct, type CoverageRow, type CoverageTrendPoint } from "./run-coverage.client";

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function stubFetch(rows: CoverageRow[], points: CoverageTrendPoint[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/coverage-trend")) {
        return new Response(JSON.stringify({ points }), { status: 200 });
      }
      return new Response(JSON.stringify({ coverage: rows }), { status: 200 });
    }),
  );
}

const row: CoverageRow = {
  job_run_id: "j1",
  job_name: "unit",
  format: "go-cover",
  lines_covered: 70,
  lines_total: 100,
  packages: [
    { name: "internal/pay", lines_covered: 10, lines_total: 40 },
    { name: "internal/cart", lines_covered: 60, lines_total: 60 },
  ],
  created_at: "2026-06-12T10:00:00Z",
};

describe("pct", () => {
  it("matches the agent's formula — one decimal of 100*covered/total", () => {
    expect(pct(70, 100)).toBe("70.0%");
    expect(pct(2, 3)).toBe("66.7%");
  });
  it("renders a dash for zero totals instead of NaN", () => {
    expect(pct(0, 0)).toBe("—");
  });
});

describe("RunCoverage", () => {
  it("renders per-job percentage and the collapsible package breakdown", async () => {
    stubFetch([row], []);
    renderWithQuery(
      <RunCoverage runId="r1" runStatus="success" pipelineId="p1" apiBaseURL="" />,
    );
    expect(await screen.findByText("70.0%")).toBeTruthy();
    expect(screen.getByText(/70 of 100 lines/)).toBeTruthy();

    fireEvent.click(screen.getByText(/2 package\(s\)/));
    await waitFor(() => {
      expect(screen.getByText("internal/pay")).toBeTruthy();
      expect(screen.getByText("25.0%")).toBeTruthy(); // 10/40
    });
  });

  it("filters the trend sparkline to the job's own series", async () => {
    const points: CoverageTrendPoint[] = [
      { run_id: "r1", job_name: "unit", lines_covered: 70, lines_total: 100, created_at: "2026-06-12T10:00:00Z" },
      { run_id: "r0", job_name: "unit", lines_covered: 60, lines_total: 100, created_at: "2026-06-11T10:00:00Z" },
      // Another job's series — must NOT join unit's sparkline.
      { run_id: "r1", job_name: "integration", lines_covered: 1, lines_total: 100, created_at: "2026-06-12T10:00:00Z" },
      { run_id: "r0", job_name: "integration", lines_covered: 99, lines_total: 100, created_at: "2026-06-11T10:00:00Z" },
    ];
    stubFetch([row], points);
    renderWithQuery(
      <RunCoverage runId="r1" runStatus="success" pipelineId="p1" apiBaseURL="" />,
    );
    const spark = await screen.findByRole("img", { name: /coverage trend/ });
    // 2 points of `unit`, never 4 — mixed-job series would say "4 runs".
    expect(spark.getAttribute("aria-label")).toContain("2 runs");
  });

  it("shows the empty-state hint when nothing reported", async () => {
    stubFetch([], []);
    renderWithQuery(
      <RunCoverage runId="r1" runStatus="success" pipelineId="p1" apiBaseURL="" />,
    );
    expect(await screen.findByText(/No coverage reported/)).toBeTruthy();
  });
});

describe("deltaPP", () => {
  const base = { run_id: "r0", lines_covered: 50, lines_total: 100 };
  it("computes percentage-point movement vs baseline", () => {
    expect(
      deltaPP({ ...row, lines_covered: 70, lines_total: 100, baseline: base }),
    ).toBeCloseTo(20);
    expect(
      deltaPP({ ...row, lines_covered: 40, lines_total: 100, baseline: base }),
    ).toBeCloseTo(-10);
  });
  it("returns null without baseline or measurable lines", () => {
    expect(deltaPP({ ...row, baseline: undefined })).toBeNull();
    expect(
      deltaPP({ ...row, lines_total: 0, baseline: base }),
    ).toBeNull();
  });
});

describe("DeltaChip rendering", () => {
  it("shows the delta vs main on the card", async () => {
    stubFetch(
      [{ ...row, baseline: { run_id: "r0", lines_covered: 80, lines_total: 100 } }],
      [],
    );
    renderWithQuery(
      <RunCoverage runId="r1" runStatus="success" pipelineId="p1" apiBaseURL="" />,
    );
    // 70% now vs 80% main → −10.0pp.
    expect(await screen.findByText("−10.0pp vs main")).toBeTruthy();
  });
});
