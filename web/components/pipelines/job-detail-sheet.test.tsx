import { act, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import type { JobDetail } from "@/types/api";

import { JobDetailSheet } from "./job-detail-sheet.client";
import type { JobDetailResult } from "@/server/actions/runs";

const fetchJobDetail = vi.fn<(i: unknown) => Promise<JobDetailResult>>();
vi.mock("@/server/actions/runs", () => ({
  fetchJobDetail: (i: unknown) => fetchJobDetail(i),
}));

// mockReset (not clearAllMocks) so the mockResolvedValueOnce queue never leaks
// into the next test — clearAllMocks clears calls but keeps the once-queue.
afterEach(() => fetchJobDetail.mockReset());

// okResult builds a JobDetailResult; `agent_id` is the distinguishing
// field rendered in the sheet's <dl> (the Agent field), so a test can tell
// the first fetch's attempt from a later one by its agent id text.
function okResult(over: Partial<JobDetail> = {}): JobDetailResult {
  return {
    ok: true,
    stageName: "build",
    job: {
      id: "j1",
      stage_run_id: "sr1",
      name: "unit",
      status: "failed",
      attempt: 0,
      agent_id: "agent-old",
      ...over,
    },
    run: {
      id: "r1",
      counter: 7,
      status: "failed",
      pipeline_name: "web",
      project_slug: "acme",
    },
  };
}

function Wrap({ open }: { open: boolean }) {
  return (
    <TooltipProvider>
      <JobDetailSheet
        runId="r1"
        jobId="j1"
        jobName="unit"
        open={open}
        onOpenChange={() => {}}
      />
    </TooltipProvider>
  );
}

describe("JobDetailSheet — re-fetch on reopen (stale after rerun)", () => {
  it("does not fetch while closed (lazy on open)", () => {
    fetchJobDetail.mockResolvedValue(okResult());
    render(<Wrap open={false} />);
    expect(fetchJobDetail).toHaveBeenCalledTimes(0);
  });

  it("re-fetches every time the sheet opens, showing the fresh attempt", async () => {
    // Reproduces the bug: a rerun bumps attempt in place (same jobRunId) while
    // this component stays mounted. Reopening must re-fetch, not show the cached
    // prior attempt.
    fetchJobDetail
      .mockResolvedValueOnce(okResult({ agent_id: "agent-old", status: "failed" }))
      .mockResolvedValueOnce(okResult({ agent_id: "agent-new", status: "running" }));

    const { rerender } = render(<Wrap open={false} />);
    expect(fetchJobDetail).toHaveBeenCalledTimes(0);

    rerender(<Wrap open />);
    expect(await screen.findByText("agent-old")).toBeTruthy();
    expect(fetchJobDetail).toHaveBeenCalledTimes(1);

    rerender(<Wrap open={false} />);
    rerender(<Wrap open />);
    expect(await screen.findByText("agent-new")).toBeTruthy();
    expect(fetchJobDetail).toHaveBeenCalledTimes(2);
    expect(screen.queryByText("agent-old")).toBeNull();
  });

  it("does not re-fetch on a re-render while staying open", async () => {
    fetchJobDetail.mockResolvedValue(okResult());
    const { rerender } = render(<Wrap open />);
    expect(await screen.findByText("agent-old")).toBeTruthy();
    rerender(<Wrap open />);
    rerender(<Wrap open />);
    expect(fetchJobDetail).toHaveBeenCalledTimes(1);
  });

  it("ignores a stale in-flight response that resolves after a reopen", async () => {
    // A fires on the first open, stays pending across a close+reopen; B fires on
    // the reopen and resolves first (attempt-2). When A finally resolves it must
    // be dropped by the `live` guard so attempt-1 never overwrites attempt-2.
    let resolveA!: (v: JobDetailResult) => void;
    let resolveB!: (v: JobDetailResult) => void;
    fetchJobDetail
      .mockImplementationOnce(() => new Promise<JobDetailResult>((r) => (resolveA = r)))
      .mockImplementationOnce(() => new Promise<JobDetailResult>((r) => (resolveB = r)));

    const { rerender } = render(<Wrap open />);
    rerender(<Wrap open={false} />);
    rerender(<Wrap open />);
    expect(fetchJobDetail).toHaveBeenCalledTimes(2);

    resolveB(okResult({ agent_id: "agent-new", status: "running" }));
    expect(await screen.findByText("agent-new")).toBeTruthy();

    // Fully flush A's late resolution: without the `live` guard this WOULD
    // call setResult(agent-old) and re-render, so the flush must be thorough
    // enough that a missing guard is caught (not a false pass on under-flush).
    await act(async () => {
      resolveA(okResult({ agent_id: "agent-old", status: "failed" }));
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(screen.queryByText("agent-old")).toBeNull();
    expect(screen.getByText("agent-new")).toBeTruthy();
  });
});
