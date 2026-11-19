import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

vi.mock("next/navigation", () => ({
  usePathname: () => "/projects/acme",
}));

import { ProjectTabs } from "./project-tabs.client";

describe("ProjectTabs", () => {
  it("renders the always-on sections", () => {
    render(<ProjectTabs slug="acme" />);
    for (const label of ["Pipelines", "VSM", "Environments", "Settings"]) {
      expect(screen.getByRole("link", { name: label })).toBeTruthy();
    }
  });

  // A project shipping a plain Deployment has no Rollout, so the tab would
  // only ever lead to a dead cluster/namespace form — hide it. Default off
  // also covers a viewer, whose maintainer-gated deploy-targets read 403s.
  it("hides Rollouts by default", () => {
    render(<ProjectTabs slug="acme" />);
    expect(screen.queryByRole("link", { name: "Rollouts" })).toBeNull();
  });

  it("shows Rollouts when the project has a rollout-aware target", () => {
    render(<ProjectTabs slug="acme" showRollouts />);
    const tab = screen.getByRole("link", { name: "Rollouts" });
    expect(tab.getAttribute("href")).toBe("/projects/acme/rollouts");
  });

  // The schedules read is maintainer-gated, so a viewer clicking the tab would
  // only hit a 403 — hide it unless the caller can manage the project.
  it("hides Schedules by default", () => {
    render(<ProjectTabs slug="acme" />);
    expect(screen.queryByRole("link", { name: "Schedules" })).toBeNull();
  });

  it("shows Schedules for a maintainer", () => {
    render(<ProjectTabs slug="acme" showSchedules />);
    const tab = screen.getByRole("link", { name: "Schedules" });
    expect(tab.getAttribute("href")).toBe("/projects/acme/crons");
  });

  it("marks the active section with aria-current", () => {
    render(<ProjectTabs slug="acme" />);
    expect(
      screen.getByRole("link", { name: "Pipelines" }).getAttribute("aria-current"),
    ).toBe("page");
  });
});
