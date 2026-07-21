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
    active_service: "",
    preview_service: "",
    scale_down_delay_seconds: 0,
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

// blueGreen is the canary fixture re-shaped into a blue-green rollout paused at
// pre-promotion, with both services, a scale-down delay and a pre-promotion analysis.
function blueGreen(partial: Partial<Rollout> = {}): Rollout {
  return canary({
    strategy: "blueGreen",
    phase: "Paused",
    message: "BlueGreenPause",
    active_service: "payments-active",
    preview_service: "payments-preview",
    scale_down_delay_seconds: 45,
    analysis: { name: "pre-promo", phase: "Successful", message: "8/8 checks" },
    ...partial,
  });
}

describe("RolloutPanel — blue-green", () => {
  it("renders the active/preview blocks with hashes, services and serving shares", () => {
    renderPanel(blueGreen());
    expect(screen.getByText("Blue-Green")).toBeTruthy();
    // Active block: hash tag + service + 100% production (NO active image — not exposed).
    expect(screen.getByText(/Active — stable1111/)).toBeTruthy();
    expect(screen.getByText("payments-active")).toBeTruthy();
    expect(screen.getByText("100% production")).toBeTruthy();
    // Preview block: hash tag + preview image + service + 0% preview only.
    expect(screen.getByText(/Preview — canary2222/)).toBeTruthy();
    expect(screen.getByText("registry/checkout-api")).toBeTruthy();
    expect(screen.getByText("payments-preview")).toBeTruthy();
    expect(screen.getByText(/0% · preview only/)).toBeTruthy();
    // No canary internals leak into the blue-green view.
    expect(screen.queryByText("you are here")).toBeNull();
    expect(screen.queryByText(/Revisions/)).toBeNull();
  });

  it("shows the pre-promotion analysis health line when present", () => {
    renderPanel(blueGreen());
    expect(screen.getByText(/pre-promo: Successful/)).toBeTruthy();
    expect(screen.getByText("8/8 checks")).toBeTruthy();
  });

  it("notes the scaleDownDelay and that Reject does not revert Git", () => {
    renderPanel(blueGreen());
    expect(screen.getByText("45s")).toBeTruthy();
    expect(screen.getByText(/does NOT revert Git/i)).toBeTruthy();
  });

  it("notes the controller default when scaleDownDelay is unset", () => {
    renderPanel(blueGreen({ scale_down_delay_seconds: 0 }));
    expect(screen.getByText(/30s \(the controller default\)/)).toBeTruthy();
  });

  it("renders gracefully with no services and no analysis", () => {
    renderPanel(
      blueGreen({ active_service: "", preview_service: "", analysis: null }),
    );
    // Missing services collapse to an em-dash placeholder, nothing crashes.
    expect(screen.getAllByText("—").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("100% production")).toBeTruthy();
    expect(screen.queryByText(/pre-promo/)).toBeNull();
  });

  it("renders a promoted (Healthy) blue-green rollout without decision affordances", () => {
    // canManage=false here anyway, but a Healthy rollout is never actionable.
    renderPanel(blueGreen({ phase: "Healthy", analysis: null }));
    expect(screen.getByText("Healthy")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Promote/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Reject/ })).toBeNull();
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
