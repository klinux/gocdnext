import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { JobCard } from "./job-card";
import type { JobDetail } from "@/types/api";

// Minimum JobDetail fixture — fields the component reads. Any
// field omitted is implicitly the "happy" default.
function makeJob(overrides: Partial<JobDetail>): JobDetail {
  return {
    id: "00000000-0000-0000-0000-000000000001",
    stage_run_id: "00000000-0000-0000-0000-0000000000aa",
    name: "lint",
    status: "running",
    started_at: "2026-06-10T12:00:00Z",
    ...overrides,
  };
}

describe("JobCard — Canceling badge", () => {
  // The badge is the persistent in-page signal that backs the
  // cancel toast — operator sees "Canceling…" right where the
  // job is, not just in a transient sonner. v0.15.1 contract:
  // cancel_requested_at is non-null while status stays running.
  it("renders Canceling… when status=running and cancel_requested_at is set", () => {
    render(
      <JobCard
        job={makeJob({
          status: "running",
          cancel_requested_at: "2026-06-10T12:01:00Z",
        })}
        runID="run-1"
      />,
    );
    expect(screen.getByText(/Canceling/i)).toBeTruthy();
  });

  // Queued path: cancel landed before dispatch. We surface the
  // badge here too so the operator doesn't re-click thinking
  // the request was lost.
  it("renders Canceling… when status=queued and cancel_requested_at is set", () => {
    render(
      <JobCard
        job={makeJob({
          status: "queued",
          started_at: undefined,
          cancel_requested_at: "2026-06-10T12:01:00Z",
        })}
        runID="run-1"
      />,
    );
    expect(screen.getByText(/Canceling/i)).toBeTruthy();
  });

  // Terminal jobs MUST NOT carry the badge — it'd be stale UI.
  // Backend keeps cancel_requested_at populated for audit after
  // a deferred cancel finalises; the UI filter is the guard.
  it("hides the badge once status is terminal", () => {
    render(
      <JobCard
        job={makeJob({
          status: "canceled",
          finished_at: "2026-06-10T12:01:05Z",
          cancel_requested_at: "2026-06-10T12:01:00Z",
        })}
        runID="run-1"
      />,
    );
    expect(screen.queryByText(/Canceling/i)).toBeNull();
  });

  // Sibling-job sanity: a running job without cancel_requested_at
  // (which is the most common case while a single sibling is
  // being canceled) renders without the badge. Stops the badge
  // from leaking to peers via an over-broad selector.
  it("hides the badge when cancel_requested_at is absent", () => {
    render(
      <JobCard
        job={makeJob({
          status: "running",
          cancel_requested_at: undefined,
        })}
        runID="run-1"
      />,
    );
    expect(screen.queryByText(/Canceling/i)).toBeNull();
  });
});

describe("JobCard — compliance enforced badge", () => {
  // A policy-injected job (reserved `_compliance_` prefix) is badged "enforced"
  // so a dev can tell it apart from the repo's own jobs.
  it("badges a `_compliance_`-prefixed job as enforced", () => {
    render(<JobCard job={makeJob({ name: "_compliance_scan" })} runID="run-1" />);
    expect(screen.getByText(/enforced/i)).toBeTruthy();
  });

  it("does not badge a repo-authored job", () => {
    render(<JobCard job={makeJob({ name: "compile" })} runID="run-1" />);
    expect(screen.queryByText(/enforced/i)).toBeNull();
  });
});
