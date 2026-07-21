import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RolloutGatePrompt } from "./rollout-gate-buttons.client";
import type { DeployWatch } from "@/types/api";

const approveRolloutGate = vi.fn(async (_i: unknown) => ({ ok: true as const }));
const rejectRolloutGate = vi.fn(async (_i: unknown) => ({ ok: true as const }));

vi.mock("@/server/actions/environments", () => ({
  approveRolloutGate: (i: unknown) => approveRolloutGate(i),
  rejectRolloutGate: (i: unknown) => rejectRolloutGate(i),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: vi.fn() }) }));

const armed: DeployWatch = {
  deployment_revision_id: "rev-9",
  environment: "production",
  version: "1.4.2",
  expected_revision: "abc0123",
  watch_started_at: "2026-07-20T10:00:00Z",
  deadline_at: "2026-07-20T10:30:00Z",
  rollout_aware: true,
  rollout_phase: "Paused",
  rollout_step_count: 5,
  gate_id: "gate-abc",
  gate_paused_step: 2,
  gate_required: 2,
  gate_approvals_now: 1,
};

afterEach(() => vi.clearAllMocks());

async function flush() {
  await Promise.resolve();
  await Promise.resolve();
}

describe("RolloutGatePrompt", () => {
  it("renders the paused-canary banner with step + quorum + buttons", () => {
    render(<RolloutGatePrompt slug="acme" watch={armed} environment="production" />);
    expect(screen.getByText("Canary paused")).toBeTruthy();
    expect(screen.getByText(/step 3\/5/)).toBeTruthy(); // 0-based step 2 → "3/5"
    expect(screen.getByText(/awaiting approval \(1\/2\)/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Approve/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Reject/ })).toBeTruthy();
  });

  it("renders nothing once the gate is decided", () => {
    const { container } = render(
      <RolloutGatePrompt slug="acme" watch={{ ...armed, gate_decision: "approved" }} environment="production" />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing when no gate is armed", () => {
    const { gate_id, ...noGate } = armed;
    void gate_id;
    const { container } = render(
      <RolloutGatePrompt slug="acme" watch={noGate} environment="production" />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("Approve echoes the gate_id + revision on confirm", async () => {
    render(<RolloutGatePrompt slug="acme" watch={armed} environment="production" />);
    fireEvent.click(screen.getByRole("button", { name: /Approve/ }));
    // Dialog confirm button (portal).
    fireEvent.click(await screen.findByRole("button", { name: /^Approve$/ }));
    await flush();
    expect(approveRolloutGate).toHaveBeenCalledWith({
      slug: "acme",
      revisionId: "rev-9",
      gateId: "gate-abc",
    });
  });

  it("Reject makes the abort-not-a-Git-revert semantics explicit", async () => {
    render(<RolloutGatePrompt slug="acme" watch={armed} environment="production" />);
    fireEvent.click(screen.getByRole("button", { name: /Reject/ }));
    expect(await screen.findByText(/traffic shifts back to the stable version/i)).toBeTruthy();
    expect(screen.getByText(/does NOT revert Git/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Abort rollout/ }));
    await flush();
    expect(rejectRolloutGate).toHaveBeenCalledWith({
      slug: "acme",
      revisionId: "rev-9",
      gateId: "gate-abc",
    });
  });
});
