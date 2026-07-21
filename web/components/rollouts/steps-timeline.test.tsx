import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { StepsTimeline } from "./steps-timeline";
import type { Rollout, RolloutStep } from "@/types/api";

function step(partial: Partial<RolloutStep>): RolloutStep {
  return { kind: "setWeight", weight: null, pause_duration: "", ...partial };
}

// checkout-api style canary: setWeight 20 → pause 5m → setWeight 40 →
// pause manual → setWeight 60 → pause 10m → setWeight 80 → setWeight 100.
const steps: RolloutStep[] = [
  step({ kind: "setWeight", weight: 20 }),
  step({ kind: "pause", pause_duration: "5m" }),
  step({ kind: "setWeight", weight: 40 }),
  step({ kind: "pause", pause_duration: "" }),
  step({ kind: "setWeight", weight: 60 }),
  step({ kind: "pause", pause_duration: "10m" }),
  step({ kind: "setWeight", weight: 80 }),
  step({ kind: "setWeight", weight: 100 }),
];

function makeRollout(partial: Partial<Rollout> = {}): Rollout {
  return {
    namespace: "production",
    name: "checkout-api",
    strategy: "canary",
    phase: "Paused",
    message: "",
    aborted: false,
    current_step_index: 3,
    current_step_known: true,
    steps,
    canary_weight: 40,
    stable_hash: "aaaa111111",
    pod_hash: "bbbb222222",
    image: "checkout-api:1.9.0",
    analysis: null,
    ...partial,
  };
}

describe("StepsTimeline", () => {
  it("marks past steps done, the current step current, and future steps pending", () => {
    render(<StepsTimeline rollout={makeRollout()} />);
    // steps 0,1,2 are before the current index (3).
    expect(screen.getAllByLabelText(/, done$/)).toHaveLength(3);
    expect(screen.getByLabelText("setWeight 20%, done")).toBeTruthy();
    expect(screen.getByLabelText("pause 5m, done")).toBeTruthy();
    // steps 4..7 are pending.
    expect(screen.getByLabelText("setWeight 60%, pending")).toBeTruthy();
    expect(screen.getByLabelText("setWeight 100%, pending")).toBeTruthy();
  });

  it("renders the current indefinite pause as a distinct manual gate with a 'you are here' badge", () => {
    render(<StepsTimeline rollout={makeRollout()} />);
    expect(screen.getByLabelText("pause manual, current")).toBeTruthy();
    expect(screen.getByText("you are here")).toBeTruthy();
    expect(screen.getByText("manual")).toBeTruthy();
  });

  it("distinguishes a timed pause label from a manual gate label", () => {
    render(<StepsTimeline rollout={makeRollout()} />);
    expect(screen.getByText("5m")).toBeTruthy(); // timed
    expect(screen.getByText("10m")).toBeTruthy(); // timed
    expect(screen.getByText("manual")).toBeTruthy(); // indefinite
  });

  it("highlights nothing when the controller has not reported the current index", () => {
    render(
      <StepsTimeline
        rollout={makeRollout({ current_step_known: false })}
      />,
    );
    expect(screen.queryByText("you are here")).toBeNull();
    expect(screen.getAllByLabelText(/, pending$/)).toHaveLength(steps.length);
    expect(screen.queryByLabelText(/, current$/)).toBeNull();
  });

  it("clears every highlight when the rollout is aborted", () => {
    render(
      <StepsTimeline
        rollout={makeRollout({ aborted: true, current_step_index: 3 })}
      />,
    );
    expect(screen.queryByText("you are here")).toBeNull();
    expect(screen.getAllByLabelText(/, pending$/)).toHaveLength(steps.length);
  });

  it("renders a hint instead of an empty row when there are no steps", () => {
    render(<StepsTimeline rollout={makeRollout({ steps: [] })} />);
    expect(screen.getByText(/no canary steps/i)).toBeTruthy();
  });
});
