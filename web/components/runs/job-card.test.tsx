import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { ReactElement } from "react";
import { JobCard } from "./job-card";
import { TooltipProvider } from "@/components/ui/tooltip";
import type { JobDetail } from "@/types/api";

// An awaiting gate mounts ApprovalButtons, which imports the approvals server
// action + sonner + a Tooltip. Mock the module boundaries so a render-only test
// stays hermetic; the Tooltip needs its provider (app root supplies it in prod).
vi.mock("@/server/actions/approvals", () => ({
  approveJob: vi.fn(async () => ({ ok: true })),
  rejectJob: vi.fn(async () => ({ ok: true })),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

function renderCard(ui: ReactElement) {
  return render(<TooltipProvider>{ui}</TooltipProvider>);
}

// Minimum JobDetail fixture — fields the component reads. Any
// field omitted is implicitly the "happy" default.
function makeJob(overrides: Partial<JobDetail>): JobDetail {
  return {
    id: "00000000-0000-0000-0000-000000000001",
    stage_run_id: "00000000-0000-0000-0000-0000000000aa",
    name: "lint",
    status: "running",
    attempt: 0,
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

describe("JobCard — freeze hold (#227)", () => {
  function heldGate(overrides: Partial<JobDetail> = {}): JobDetail {
    return makeJob({
      status: "awaiting_approval",
      approval_gate: true,
      started_at: undefined,
      held_by_freeze: true,
      frozen_envs: ["production"],
      ...overrides,
    });
  }

  it("shows the On hold badge and disables Approve (Reject stays enabled) when a governed env is frozen", () => {
    renderCard(<JobCard job={heldGate()} runID="run-1" />);
    expect(screen.getByText(/On hold/i)).toBeTruthy();
    expect(screen.getByText(/production frozen/i)).toBeTruthy();

    const approve = screen.getByRole("button", { name: /Approve/i });
    expect((approve as HTMLButtonElement).disabled).toBe(true);
    const reject = screen.getByRole("button", { name: /Reject/i });
    expect((reject as HTMLButtonElement).disabled).toBe(false);
  });

  it("collapses several frozen envs to a count in the visible label", () => {
    renderCard(
      <JobCard job={heldGate({ frozen_envs: ["staging", "production"] })} runID="run-1" />,
    );
    expect(screen.getByText(/2 environments frozen/i)).toBeTruthy();
  });

  // Unfreeze (true → false): the badge clears and Approve re-enables (review #4).
  it("clears the badge and re-enables Approve on unfreeze", () => {
    const { rerender } = renderCard(<JobCard job={heldGate()} runID="run-1" />);
    expect(screen.getByText(/On hold/i)).toBeTruthy();

    rerender(
      <TooltipProvider>
        <JobCard
          job={heldGate({ held_by_freeze: false, frozen_envs: undefined })}
          runID="run-1"
        />
      </TooltipProvider>,
    );
    expect(screen.queryByText(/On hold/i)).toBeNull();
    const approve = screen.getByRole("button", { name: /Approve/i });
    expect((approve as HTMLButtonElement).disabled).toBe(false);
  });
});

describe("JobCard — log counter (Logs X of Y)", () => {
  const tailLogs = Array.from({ length: 5 }, (_, i) => ({
    seq: 596 + i,
    stream: "stdout",
    at: "2026-06-10T12:00:00Z",
    text: `line ${596 + i}`,
  }));

  // The bug: a tail-only response (no head/omitted) rendered "5 of 5"
  // while the visible lines were seq 596-600. logs_total keeps it honest.
  it("uses logs_total for the total, not the fetched window", () => {
    renderCard(
      <JobCard
        job={makeJob({ status: "success", logs: tailLogs, logs_total: 602 })}
        runID="run-1"
      />,
    );
    expect(screen.getByText(/Logs \(5 of 602\)/)).toBeTruthy();
  });

  // Older server without logs_total: fall back to visible + omitted.
  it("falls back to window + omitted when logs_total is absent", () => {
    renderCard(
      <JobCard
        job={makeJob({ status: "success", logs: tailLogs, logs_omitted: 10 })}
        runID="run-1"
      />,
    );
    expect(screen.getByText(/Logs \(5 of 15\)/)).toBeTruthy();
  });
});
