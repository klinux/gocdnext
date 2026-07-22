import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RolloutGateButtons, RolloutGatePrompt } from "./rollout-gate-buttons.client";
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
  gate_rollout_cluster: "prod-hub",
  gate_rollout_namespace: "shop",
  gate_rollout_name: "shop-canary",
};

afterEach(() => vi.clearAllMocks());

async function flush() {
  await Promise.resolve();
  await Promise.resolve();
}

describe("RolloutGatePrompt (Environments card — reports, does not act)", () => {
  it("renders the paused-canary banner with step + quorum", () => {
    render(<RolloutGatePrompt slug="acme" watch={armed} canManage />);
    expect(screen.getByText("Canary paused")).toBeTruthy();
    expect(screen.getByText(/step 3\/5/)).toBeTruthy(); // 0-based step 2 → "3/5"
    expect(screen.getByText(/awaiting approval \(1\/2\)/)).toBeTruthy();
  });

  // Control lives in the Rollouts tab, where the steps, traffic split and analysis that
  // should justify the decision are visible. The card only reports.
  it("offers no Approve/Reject buttons", () => {
    render(<RolloutGatePrompt slug="acme" watch={armed} canManage />);
    expect(screen.queryByRole("button", { name: /Approve/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Reject/ })).toBeNull();
  });

  // The destination carries control actions and a namespace can hold several Rollouts,
  // so the link must name the PINNED one — being one panel off is not cosmetic.
  it("links to the exact pinned rollout (cluster + namespace + name)", () => {
    render(<RolloutGatePrompt slug="acme" watch={armed} canManage />);
    const link = screen.getByRole("link", { name: /Review and decide/ });
    expect(link.getAttribute("href")).toBe(
      "/projects/acme/rollouts?cluster=prod-hub&namespace=shop&name=shop-canary",
    );
  });

  // A viewer still needs to know a decision is pending, but the rollouts read is
  // maintainer-gated and the tab is hidden for them — a link would be a dead end.
  it("shows the notice without a link when the user cannot manage", () => {
    render(<RolloutGatePrompt slug="acme" watch={armed} />);
    expect(screen.getByText("Canary paused")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });

  // The pinned identity is maintainer-only on the wire, so it can be absent even for a
  // manager if the API sanitised it — never build a half-formed link.
  it("omits the link when the pinned identity is missing", () => {
    const { gate_rollout_cluster: _c, gate_rollout_namespace: _n, ...bare } = armed;
    render(<RolloutGatePrompt slug="acme" watch={bare} canManage />);
    expect(screen.getByText("Canary paused")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("renders nothing once the gate is decided", () => {
    const { container } = render(
      <RolloutGatePrompt slug="acme" watch={{ ...armed, gate_decision: "approved" }} canManage />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing when no gate is armed", () => {
    const { gate_id: _drop, ...noGate } = armed;
    const { container } = render(<RolloutGatePrompt slug="acme" watch={noGate} canManage />);
    expect(container.firstChild).toBeNull();
  });
});

// The vote wiring itself is unchanged — it just lives only in the Rollouts tab now,
// which renders RolloutGateButtons directly.
describe("RolloutGateButtons (Rollouts tab — the single place to act)", () => {
  it("Approve echoes the gate_id + revision on confirm", async () => {
    render(<RolloutGateButtons slug="acme" revisionId="rev-9" gateId="gate-abc" environment="production" />);
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
    render(<RolloutGateButtons slug="acme" revisionId="rev-9" gateId="gate-abc" environment="production" />);
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
