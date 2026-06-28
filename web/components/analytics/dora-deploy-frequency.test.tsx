import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { DoraDay } from "@/server/queries/analytics";

import { DoraDeployFrequency } from "./dora-deploy-frequency";

function day(over: Partial<DoraDay>): DoraDay {
  return {
    day: "2026-06-01",
    deploys_success: 0,
    deploys_total: 0,
    deploys_failed: 0,
    lead_time_p50_seconds: 0,
    ...over,
  };
}

const daily: DoraDay[] = [
  day({ deploys_success: 2, deploys_total: 2, deploys_failed: 0 }), // all ok
  // A successful rollback: status='success' but is_rollback → deploys_failed=1.
  // Must read as a change failure (red), matching CFR — not green.
  day({ deploys_success: 1, deploys_total: 1, deploys_failed: 1 }),
  day({ deploys_success: 1, deploys_total: 3, deploys_failed: 2 }), // 1 ok, 2 failed
  day({}), // empty day (zero-filled)
];

describe("DoraDeployFrequency", () => {
  it("counts deploys_failed (incl. rollback) as the red segment, like CFR", () => {
    render(<DoraDeployFrequency daily={daily} windowDays={30} freqPerDay={0.1} />);
    // failures = 0+1+2 = 3; total = 2+1+3 = 6.
    expect(screen.getByText("3 change failures in 6 deploys")).toBeTruthy();
  });

  it("renders the window axis and the average pill", () => {
    render(<DoraDeployFrequency daily={daily} windowDays={30} freqPerDay={1.9} />);
    expect(screen.getByText("30d ago")).toBeTruthy();
    expect(screen.getByText("today")).toBeTruthy();
    expect(screen.getByText("1.9/day")).toBeTruthy();
  });
});
