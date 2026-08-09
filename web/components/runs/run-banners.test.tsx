import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import {
  PRHeadConfigBanner,
  prHeadConfigFromCauseDetail,
} from "./run-banners";

describe("PR head config banner", () => {
  it("derives config only from pr_head cause detail", () => {
    expect(
      prHeadConfigFromCauseDetail({
        config_source: "base",
        config_revision: "abcdef1234567890",
      }),
    ).toBeNull();

    const config = prHeadConfigFromCauseDetail({
      config_source: "pr_head",
      config_revision: " abcdef1234567890 ",
      pr_url: "https://github.com/acme/widget/pull/42",
      pr_number: 42,
    });

    expect(config).toEqual({
      configRevision: "abcdef1234567890",
      prURL: "https://github.com/acme/widget/pull/42",
      prNumber: 42,
    });
  });

  it("renders the provenance warning with the short config revision and PR link", () => {
    render(
      <PRHeadConfigBanner
        config={{
          configRevision: "abcdef1234567890",
          prURL: "https://github.com/acme/widget/pull/42",
          prNumber: 42,
        }}
      />,
    );

    expect(screen.getByText("PR head config")).toBeTruthy();
    expect(
      screen.getByText(/pipeline definition from the PR head/i),
    ).toBeTruthy();
    expect(screen.getAllByText("abcdef1").length).toBeGreaterThan(0);
    const link = screen.getByRole("link", { name: "PR #42" });
    expect(link.getAttribute("href")).toBe(
      "https://github.com/acme/widget/pull/42",
    );
  });

  it("does not render unsafe PR URLs as links", () => {
    render(
      <PRHeadConfigBanner
        config={{
          configRevision: "abcdef1234567890",
          prURL: "javascript:alert(1)",
          prNumber: 42,
        }}
      />,
    );

    expect(screen.queryByRole("link", { name: "PR #42" })).toBeNull();
    expect(screen.getByText("PR #42")).toBeTruthy();
  });

  it("drops malformed cause_detail values without hiding pr_head provenance", () => {
    const config = prHeadConfigFromCauseDetail({
      config_source: "pr_head",
      config_revision: "",
      pr_url: "javascript:alert(1)",
      pr_number: -1,
    });

    expect(config).toEqual({
      configRevision: undefined,
      prURL: undefined,
      prNumber: undefined,
    });
    expect(config).not.toBeNull();
    if (!config) throw new Error("expected pr_head config");

    render(<PRHeadConfigBanner config={config} />);
    expect(screen.getByText("PR head config")).toBeTruthy();
    expect(
      screen.getByText(/pipeline definition from the PR head/i),
    ).toBeTruthy();
  });
});
