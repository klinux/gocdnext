import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { RolloutPanel } from "./rollout-panel";
import type { Rollout } from "@/types/api";

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
    render(<RolloutPanel rollout={canary()} />);
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
    render(<RolloutPanel rollout={canary({ current_step_known: false })} />);
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
      render(
        <RolloutPanel
          rollout={canary({ phase: c.phase, aborted: c.aborted ?? false })}
        />,
      );
      expect(screen.getByText(c.label)).toBeTruthy();
    });
  }
});

describe("RolloutPanel — blue-green", () => {
  it("renders a compact placeholder (PR-D deferred) instead of the canary body", () => {
    render(
      <RolloutPanel
        rollout={canary({ strategy: "blueGreen", phase: "Healthy" })}
      />,
    );
    expect(screen.getByText("Blue-Green")).toBeTruthy();
    expect(screen.getByText(/active \/ preview view is coming/i)).toBeTruthy();
    // No canary internals leak into the blue-green view.
    expect(screen.queryByText("you are here")).toBeNull();
    expect(screen.queryByText(/Revisions/)).toBeNull();
  });
});
