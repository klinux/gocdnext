import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { RolloutPanel } from "./rollout-panel";
import type { Rollout } from "@/types/api";

// The panel now renders RolloutActions (a client component pulling in the gate buttons,
// server actions, router and toast). These tests focus on the body/header rendering, so
// stub those boundaries; the action wiring is covered in rollout-actions.test.tsx.
vi.mock("@/server/actions/environments", () => ({
  promoteRollout: vi.fn(async () => ({ ok: true })),
  abortRollout: vi.fn(async () => ({ ok: true })),
  approveRolloutGate: vi.fn(async () => ({ ok: true })),
  rejectRolloutGate: vi.fn(async () => ({ ok: true })),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: vi.fn() }) }));

// renderPanel supplies the control-action props (the tests here don't exercise actions).
function renderPanel(rollout: Rollout) {
  return render(
    <RolloutPanel rollout={rollout} slug="acme" cluster="prod" canManage={false} />,
  );
}

function canary(partial: Partial<Rollout> = {}): Rollout {
  return {
    namespace: "production",
    name: "checkout-api",
    strategy: "canary",
    phase: "Paused",
    message: "waiting for manual promotion",
    aborted: false,
    current_step_index: 3,
    current_step_known: true,
    steps: [
      { kind: "setWeight", weight: 20, pause_duration: "" },
      { kind: "pause", weight: null, pause_duration: "5m" },
      { kind: "setWeight", weight: 40, pause_duration: "" },
      { kind: "pause", weight: null, pause_duration: "" },
      { kind: "setWeight", weight: 60, pause_duration: "" },
      { kind: "setWeight", weight: 100, pause_duration: "" },
    ],
    canary_weight: 40,
    stable_hash: "stable1111a",
    pod_hash: "canary2222b",
    image: "registry/checkout-api:1.9.0",
    analysis: { name: "success-rate", phase: "Running", message: "5/8 measurements" },
    ...partial,
  };
}

describe("RolloutPanel — canary", () => {
  it("renders the strategy pill, name, meta and the canary body", () => {
    renderPanel(canary());
    // "Canary" shows twice: the strategy pill + the canary revision role.
    expect(screen.getAllByText("Canary").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByRole("heading", { name: "checkout-api" })).toBeTruthy();
    expect(screen.getByText("ns/production")).toBeTruthy();
    expect(screen.getByText("step 4/6")).toBeTruthy(); // 6 steps, current index 3
    // Revision strip + traffic + steps + analysis are all present.
    expect(screen.getByText("Stable")).toBeTruthy();
    expect(screen.getByRole("img", { name: /canary 40%, stable 60%/ })).toBeTruthy();
    expect(screen.getByText("you are here")).toBeTruthy();
    expect(screen.getByText("success-rate")).toBeTruthy();
    expect(screen.getByText("Running")).toBeTruthy();
  });

  it("shows '?' in the meta step counter when the current index is unknown", () => {
    renderPanel(canary({ current_step_known: false }));
    expect(screen.getByText("step ?/6")).toBeTruthy();
  });
});

describe("RolloutPanel — status pill per phase", () => {
  const cases: { phase: string; aborted?: boolean; label: string }[] = [
    { phase: "Paused", label: "Paused" },
    { phase: "Progressing", label: "Progressing" },
    { phase: "Healthy", label: "Healthy" },
    { phase: "Degraded", label: "Degraded" },
    { phase: "Progressing", aborted: true, label: "Aborted" },
  ];
  for (const c of cases) {
    it(`renders "${c.label}" for phase=${c.phase}${c.aborted ? " aborted" : ""}`, () => {
      renderPanel(canary({ phase: c.phase, aborted: c.aborted ?? false }));
      expect(screen.getByText(c.label)).toBeTruthy();
    });
  }
});

describe("RolloutPanel — blue-green", () => {
  it("renders a compact placeholder (PR-D deferred) instead of the canary body", () => {
    renderPanel(canary({ strategy: "blueGreen", phase: "Healthy" }));
    expect(screen.getByText("Blue-Green")).toBeTruthy();
    expect(screen.getByText(/active \/ preview view is coming/i)).toBeTruthy();
    // No canary internals leak into the blue-green view.
    expect(screen.queryByText("you are here")).toBeNull();
    expect(screen.queryByText(/Revisions/)).toBeNull();
  });
});

describe("RolloutPanel — unknown strategy", () => {
  it("renders a neutral placeholder for a strategy-less Rollout, not blue-green", () => {
    renderPanel(canary({ strategy: "", phase: "Progressing" }));
    expect(screen.getByText("Unknown")).toBeTruthy();
    expect(screen.getByText(/No recognised rollout strategy/i)).toBeTruthy();
    // A strategy-less rollout must NOT be mislabelled blue-green, and no canary
    // internals leak.
    expect(screen.queryByText("Blue-Green")).toBeNull();
    expect(screen.queryByText("you are here")).toBeNull();
  });
});
