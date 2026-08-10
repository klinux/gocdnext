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

  // Labelled by the run's cause, not a cause_detail marker — so a schedule that
  // resolved an upstream is never mislabelled as a manual re-deploy.
  it("labels by cause and surfaces the resolved build", () => {
    const cases: Array<[string | undefined, RegExp]> = [
      ["upstream", /Triggered by/i],
      ["manual", /Manual re-deploy of/i],
      ["schedule", /Scheduled re-deploy of/i],
    ];
    for (const [cause, label] of cases) {
      const { unmount } = render(<UpstreamBanner upstream={base} cause={cause} />);
      expect(screen.getByText(label)).toBeTruthy();
      expect(screen.getByText("build")).toBeTruthy();
      expect(
        screen.getByRole("link", { name: /#14/ }).getAttribute("href"),
      ).toBe("/runs/run-14");
      unmount();
    }
  });

  // A manual re-deploy is not mislabelled "Triggered by".
  it("does not label a manual re-deploy as triggered-by", () => {
    render(<UpstreamBanner upstream={base} cause="manual" />);
    expect(screen.queryByText(/Triggered by/i)).toBeNull();
  });
});
