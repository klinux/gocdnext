import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { CauseBadge } from "./cause-badge";

describe("CauseBadge", () => {
  // Each run's trigger cause gets a distinct, human label (and icon)
  // so a PR run reads differently from a push/tag/manual at a glance —
  // the plain `cause` string ("pull_request") was ambiguous in the UI.
  it.each([
    ["push", "Push"],
    ["pull_request", "PR"],
    ["tag", "Tag"],
    ["manual", "Manual"],
    ["schedule", "Schedule"],
    ["cron", "Schedule"],
    ["upstream", "Upstream"],
  ])("renders cause %s as label %s", (cause, label) => {
    render(<CauseBadge cause={cause} />);
    expect(screen.getByText(label)).toBeTruthy();
  });

  it("falls back to the raw cause for an unknown value (forward-compatible)", () => {
    render(<CauseBadge cause="webhook" />);
    expect(screen.getByText("webhook")).toBeTruthy();
  });
});
