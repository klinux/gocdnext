import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { RunSecurityPanel, type RunSecurityData } from "./run-security.client";

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function stub(data: Partial<RunSecurityData>) {
  const full: RunSecurityData = {
    has_scans: true,
    delta_available: false,
    unbaselined_series: 0,
    critical: 0,
    high: 0,
    medium: 0,
    low: 0,
    open_total: 0,
    accepted: 0,
    new_in_change: [],
    ...data,
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(full), { status: 200 })),
  );
}

describe("RunSecurityPanel", () => {
  it("shows the empty-state hint when the run was never scanned", async () => {
    stub({ has_scans: false });
    renderWithQuery(<RunSecurityPanel runId="r1" runStatus="success" apiBaseURL="" />);
    expect(await screen.findByText(/No security scan reported/)).toBeTruthy();
  });

  it("lists new-in-change findings when a base is comparable", async () => {
    stub({
      delta_available: true,
      high: 1,
      open_total: 1,
      new_in_change: [
        {
          scanner_job: "scan",
          matrix_key: "",
          tool: "Trivy",
          rule_id: "CVE-1",
          severity: "high",
          message: "boom",
          location_path: "go.sum",
          location_line: 3,
        },
      ],
    });
    renderWithQuery(<RunSecurityPanel runId="r1" runStatus="success" apiBaseURL="" />);
    expect(await screen.findByText("1 new in this change")).toBeTruthy();
    expect(screen.getByText("CVE-1")).toBeTruthy();
  });

  it("says no comparable base when delta is unavailable", async () => {
    stub({ delta_available: false, high: 2, open_total: 2 });
    renderWithQuery(<RunSecurityPanel runId="r1" runStatus="success" apiBaseURL="" />);
    expect(await screen.findByText(/No comparable base scan/)).toBeTruthy();
    expect(screen.getByText("2 High")).toBeTruthy();
  });

  it("shows accepted separately from open", async () => {
    stub({ delta_available: true, accepted: 3, open_total: 0 });
    renderWithQuery(<RunSecurityPanel runId="r1" runStatus="success" apiBaseURL="" />);
    expect(await screen.findByText("3 accepted")).toBeTruthy();
    expect(screen.getByText("0 open findings")).toBeTruthy();
  });
});
