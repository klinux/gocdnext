import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { UpstreamBanner } from "./run-banners";

describe("UpstreamBanner", () => {
  const base = {
    upstream_run_id: "run-14",
    upstream_pipeline: "build",
    upstream_stage: "image",
    upstream_run_counter: 14,
  };

  // A fanout-triggered run reads as "Triggered by".
  it("labels a fanout run as triggered by its upstream", () => {
    render(<UpstreamBanner upstream={base} />);
    expect(screen.getByText(/Triggered by/i)).toBeTruthy();
    expect(screen.getByText("build")).toBeTruthy();
    expect(
      screen.getByRole("link", { name: /#14/ }).getAttribute("href"),
    ).toBe("/runs/run-14");
  });

  // A manual re-deploy resolved the latest build — the operator must see WHICH
  // build shipped, and that it was a hand-kick rather than a fanout.
  it("labels a manual re-deploy and surfaces the resolved build", () => {
    render(<UpstreamBanner upstream={{ ...base, manual_upstream: true }} />);
    expect(screen.getByText(/Manual re-deploy of/i)).toBeTruthy();
    expect(screen.queryByText(/Triggered by/i)).toBeNull();
    expect(screen.getByText("build")).toBeTruthy();
    expect(
      screen.getByRole("link", { name: /#14/ }).getAttribute("href"),
    ).toBe("/runs/run-14");
  });
});
