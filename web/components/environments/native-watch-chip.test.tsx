import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { NativeWatchChip } from "./native-watch-chip.client";
import type { DeployWatch } from "@/types/api";

const base: DeployWatch = {
  deployment_revision_id: "rev-1",
  environment: "production",
  version: "1.4.2",
  expected_revision: "abc0123456789def",
  watch_started_at: "2026-07-12T10:00:00Z",
  deadline_at: "2026-07-12T10:15:00Z",
};

describe("NativeWatchChip", () => {
  it("shows Syncing with a truncated revision once the sync has fired", () => {
    render(
      <NativeWatchChip watch={{ ...base, sync_requested_at: "2026-07-12T10:00:05Z" }} />,
    );
    expect(screen.getByText("Syncing")).toBeTruthy();
    expect(screen.getByText("abc0123")).toBeTruthy(); // 7-char short SHA
  });

  it("reads Deploying before the sync fires (no sync_requested_at)", () => {
    render(<NativeWatchChip watch={base} />);
    expect(screen.getByText("Deploying")).toBeTruthy();
  });

  it("shows Degraded (and it wins over Syncing) when the rollout is degraded", () => {
    render(
      <NativeWatchChip
        watch={{
          ...base,
          sync_requested_at: "2026-07-12T10:00:05Z",
          degraded_since: "2026-07-12T10:01:00Z",
        }}
      />,
    );
    expect(screen.getByText("Degraded")).toBeTruthy();
    expect(screen.queryByText("Syncing")).toBeNull();
  });

  it("shows canary progress with a KNOWN step (step 0 renders as a definite step, not ?)", () => {
    render(
      <NativeWatchChip
        watch={{
          ...base,
          sync_requested_at: "2026-07-12T10:00:05Z",
          rollout_aware: true,
          rollout_phase: "Paused",
          rollout_current_step: 0, // known → definite
          rollout_step_count: 8,
        }}
      />,
    );
    expect(screen.getByText(/Canary paused/)).toBeTruthy();
    expect(screen.getByText("step 1/8")).toBeTruthy(); // 0-based index 0 → 1-based "1", not "?"
    expect(screen.queryByText("Syncing")).toBeNull();
  });

  it("renders an UNKNOWN step as ? (controller hasn't reported the index)", () => {
    render(
      <NativeWatchChip
        watch={{
          ...base,
          rollout_aware: true,
          rollout_phase: "Progressing",
          rollout_step_count: 8, // no rollout_current_step
        }}
      />,
    );
    expect(screen.getByText(/Rolling out/)).toBeTruthy();
    expect(screen.getByText("step ?/8")).toBeTruthy();
  });

  it("surfaces a rollout read error without leaking internals", () => {
    render(
      <NativeWatchChip
        watch={{ ...base, rollout_aware: true, rollout_error: "the deploy could not be observed" }}
      />,
    );
    expect(screen.getByText(/Rollout status unavailable/)).toBeTruthy();
  });

  it("falls through to Deploying when rollout-aware but not yet observed", () => {
    render(<NativeWatchChip watch={{ ...base, rollout_aware: true }} />);
    expect(screen.getByText("Deploying")).toBeTruthy();
  });

  it("surfaces an active AnalysisRun alongside the canary chip", () => {
    render(
      <NativeWatchChip
        watch={{
          ...base,
          rollout_aware: true,
          rollout_phase: "Paused",
          rollout_pause_reason: "AnalysisRunInconclusive",
          rollout_analysis_phase: "Inconclusive",
          rollout_analysis_message: "success-rate 0.91 < 0.95",
        }}
      />,
    );
    expect(screen.getByText(/Canary paused/)).toBeTruthy();
    const badge = screen.getByText(/analysis inconclusive/);
    expect(badge).toBeTruthy();
    expect(badge.getAttribute("title")).toContain("success-rate 0.91");
  });

  it("shows no analysis badge when none is running", () => {
    render(
      <NativeWatchChip
        watch={{ ...base, rollout_aware: true, rollout_phase: "Progressing" }} />,
    );
    expect(screen.queryByText(/^analysis /)).toBeNull();
  });

  // The correlation anchor must survive the states that REPLACE the generic chip
  // text — those are exactly when a stalled deploy needs debugging, and the version
  // label alone no longer reveals which commit the watch is waiting on.
  it("keeps the correlation revision visible in the rollout state", () => {
    render(
      <NativeWatchChip
        watch={{ ...base, rollout_aware: true, rollout_phase: "Paused", rollout_step_count: 5, rollout_current_step: 2 }}
      />,
    );
    expect(screen.getByText("Canary paused")).toBeTruthy();
    expect(screen.getByText("abc0123")).toBeTruthy();
  });

  it("keeps the correlation revision visible in the degraded state", () => {
    render(<NativeWatchChip watch={{ ...base, degraded_since: "2026-07-12T10:05:00Z" }} />);
    expect(screen.getByText("Degraded")).toBeTruthy();
    expect(screen.getByText("abc0123")).toBeTruthy();
  });

  it("omits the anchor badge when there is no expected revision", () => {
    render(
      <NativeWatchChip
        watch={{ ...base, expected_revision: "", rollout_aware: true, rollout_phase: "Paused" }}
      />,
    );
    expect(screen.queryByText("abc0123")).toBeNull();
  });
});
