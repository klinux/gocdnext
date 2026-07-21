import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RolloutActions } from "./rollout-actions.client";
import type { Rollout } from "@/types/api";

const promoteRollout = vi.fn(async (_i: unknown) => ({ ok: true as const }));
const abortRollout = vi.fn(async (_i: unknown) => ({ ok: true as const }));
const approveRolloutGate = vi.fn(async (_i: unknown) => ({ ok: true as const }));
const rejectRolloutGate = vi.fn(async (_i: unknown) => ({ ok: true as const }));

vi.mock("@/server/actions/environments", () => ({
  promoteRollout: (i: unknown) => promoteRollout(i),
  abortRollout: (i: unknown) => abortRollout(i),
  approveRolloutGate: (i: unknown) => approveRolloutGate(i),
  rejectRolloutGate: (i: unknown) => rejectRolloutGate(i),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: vi.fn() }) }));

function canary(partial: Partial<Rollout> = {}): Rollout {
  return {
    namespace: "production",
    name: "checkout-api",
    strategy: "canary",
    phase: "Paused",
    message: "",
    aborted: false,
    current_step_index: 3,
    current_step_known: true,
    steps: [{ kind: "pause", weight: null, pause_duration: "" }],
    canary_weight: 40,
    stable_hash: "stable1",
    pod_hash: "canary1",
    image: "reg/checkout:1.9.0",
    analysis: null,
    ...partial,
  };
}

afterEach(() => vi.clearAllMocks());

async function flush() {
  await Promise.resolve();
  await Promise.resolve();
}

describe("RolloutActions — gated rollout", () => {
  const gated = canary({
    gate: {
      gate_id: "gate-1",
      revision_id: "rev-7",
      approvals_now: 1,
      required: 2,
    },
  });

  it("offers Approve/Reject (the vote path) + quorum, and NOT Promote/Abort", () => {
    render(
      <RolloutActions slug="acme" cluster="prod" canManage rollout={gated} />,
    );
    expect(screen.getByRole("button", { name: /Approve/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Reject/ })).toBeTruthy();
    expect(screen.getByText(/awaiting approval \(1\/2\)/)).toBeTruthy();
    // A gated rollout must never expose a direct bypass.
    expect(screen.queryByRole("button", { name: /^Promote$/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Abort$/ })).toBeNull();
  });

  it("Approve echoes the gate's revision_id + gate_id", async () => {
    render(
      <RolloutActions slug="acme" cluster="prod" canManage rollout={gated} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Approve/ }));
    fireEvent.click(await screen.findByRole("button", { name: /^Approve$/ }));
    await flush();
    expect(approveRolloutGate).toHaveBeenCalledWith({
      slug: "acme",
      revisionId: "rev-7",
      gateId: "gate-1",
    });
    expect(promoteRollout).not.toHaveBeenCalled();
  });
});

describe("RolloutActions — non-gated actionable canary", () => {
  it("offers Promote/Abort (not Approve/Reject) when a manager sees a paused canary", () => {
    render(
      <RolloutActions slug="acme" cluster="prod" canManage rollout={canary()} />,
    );
    expect(screen.getByRole("button", { name: /Promote/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Abort/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Approve/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Reject/ })).toBeNull();
  });

  it("also offers actions while Progressing", () => {
    render(
      <RolloutActions
        slug="acme"
        cluster="prod"
        canManage
        rollout={canary({ phase: "Progressing" })}
      />,
    );
    expect(screen.getByRole("button", { name: /Promote/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Abort/ })).toBeTruthy();
  });

  it("Promote confirm calls promoteRollout with the rollout identity", async () => {
    const onActed = vi.fn();
    render(
      <RolloutActions
        slug="acme"
        cluster="prod"
        canManage
        rollout={canary()}
        onActed={onActed}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Promote/ }));
    fireEvent.click(await screen.findByRole("button", { name: /^Promote$/ }));
    await flush();
    expect(promoteRollout).toHaveBeenCalledWith({
      slug: "acme",
      cluster: "prod",
      namespace: "production",
      name: "checkout-api",
    });
    expect(onActed).toHaveBeenCalled();
  });

  it("the Abort dialog states it does NOT revert Git, and confirm calls abortRollout", async () => {
    render(
      <RolloutActions slug="acme" cluster="prod" canManage rollout={canary()} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Abort/ }));
    expect(
      await screen.findByText(/traffic shifts back to the stable version/i),
    ).toBeTruthy();
    expect(screen.getByText(/does NOT revert Git/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Abort rollout/ }));
    await flush();
    expect(abortRollout).toHaveBeenCalledWith({
      slug: "acme",
      cluster: "prod",
      namespace: "production",
      name: "checkout-api",
    });
  });
});

describe("RolloutActions — nothing to offer", () => {
  it("renders nothing for a non-manager (server is the authority, but hide the affordance)", () => {
    const { container } = render(
      <RolloutActions
        slug="acme"
        cluster="prod"
        canManage={false}
        rollout={canary()}
      />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing for a healthy (non-actionable) canary with no gate", () => {
    const { container } = render(
      <RolloutActions
        slug="acme"
        cluster="prod"
        canManage
        rollout={canary({ phase: "Healthy" })}
      />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("shows no gate buttons when the gate is absent/decided (server omits a decided gate)", () => {
    render(
      <RolloutActions slug="acme" cluster="prod" canManage rollout={canary()} />,
    );
    expect(screen.queryByRole("button", { name: /Approve/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Reject/ })).toBeNull();
  });
});
