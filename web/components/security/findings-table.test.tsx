import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { FindingsTable } from "./findings-table";
import type { Finding } from "@/types/api";

// FindingsTable renders the interactive FindingStateMenu (a client island) which
// needs a router + the action; stub them so the presentational assertions run.
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: vi.fn() }) }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@/server/actions/security", () => ({ setFindingState: vi.fn() }));

function finding(over: Partial<Finding> = {}): Finding {
  return {
    id: 1,
    pipeline_id: "p",
    run_id: "r",
    job_name: "scan",
    tool: "Trivy",
    rule_id: "CVE-2023-1234",
    severity: "critical",
    level: "error",
    message: "openssl buffer overflow",
    location_path: "go.sum",
    location_line: 12,
    location_url: "go.sum",
    artifact_path: "trivy.sarif",
    created_at: "2026-06-29T00:00:00Z",
    status: "existing",
    state: "open",
    state_id: 100,
    state_reason: "",
    ...over,
  };
}

describe("FindingsTable", () => {
  it("renders a finding with severity badge, rule, location, job", () => {
    render(<FindingsTable findings={[finding()]} slug="demo" />);
    expect(screen.getByText("Critical")).toBeTruthy();
    expect(screen.getByText("CVE-2023-1234")).toBeTruthy();
    expect(screen.getByText("Trivy")).toBeTruthy();
    expect(screen.getByText("go.sum:12")).toBeTruthy();
    expect(screen.getByText("scan")).toBeTruthy();
  });

  it("shows an em-dash when there's no location", () => {
    render(<FindingsTable findings={[finding({ id: 2, location_path: "", location_line: 0 })]} slug="demo" />);
    expect(screen.getByText("—")).toBeTruthy();
  });

  it("badges a finding first seen in this run as New", () => {
    render(<FindingsTable findings={[finding({ id: 3, status: "new" })]} slug="demo" />);
    expect(screen.getByText("New")).toBeTruthy();
  });

  it("does not badge an existing finding", () => {
    render(<FindingsTable findings={[finding({ id: 4, status: "existing" })]} slug="demo" />);
    expect(screen.queryByText("New")).toBeNull();
  });
});
