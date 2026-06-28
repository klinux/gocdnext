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
  day({ deploys_success: 2, deploys_total: 2 }), // all ok
  day({ deploys_success: 1, deploys_total: 3 }), // 1 ok, 2 failed
  day({}), // empty day (zero-filled)
];

describe("DoraDeployFrequency", () => {
  it("summarizes failures over total deploys (fail = total − success)", () => {
    render(<DoraDeployFrequency daily={daily} windowDays={30} freqPerDay={0.1} />);
    // 2 failed in (2+3) = 5 total.
    expect(screen.getByText("2 failures in 5 deploys")).toBeTruthy();
  });

  it("renders the window axis and the average pill", () => {
    render(<DoraDeployFrequency daily={daily} windowDays={30} freqPerDay={1.9} />);
    expect(screen.getByText("30d ago")).toBeTruthy();
    expect(screen.getByText("today")).toBeTruthy();
    expect(screen.getByText("1.9/day")).toBeTruthy();
  });
});
