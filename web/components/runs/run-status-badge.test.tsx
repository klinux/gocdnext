import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { RunStatusBadge } from "./run-status-badge";

describe("RunStatusBadge", () => {
  it("renders a linked 'superseded by #N' badge for a superseded run", () => {
    render(
      <RunStatusBadge
        status="canceled"
        cancelReason="superseded by #5"
        supersededBy="run-5-id"
      />,
    );
    const link = screen.getByRole("link", { name: /superseded by #5/i });
    expect(link.getAttribute("href")).toBe("/runs/run-5-id");
  });

  it("shows the superseded badge without a link when the winning run is gone", () => {
    render(<RunStatusBadge status="canceled" cancelReason="superseded by #5" />);
    expect(screen.getByText("superseded by #5")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("renders the normal status badge for a live run", () => {
    render(<RunStatusBadge status="running" />);
    expect(screen.getByText(/running/i)).toBeTruthy();
    expect(screen.queryByText(/superseded/i)).toBeNull();
  });

  it("renders a plain Canceled badge when canceled but not superseded", () => {
    render(<RunStatusBadge status="canceled" />);
    expect(screen.getByText(/canceled/i)).toBeTruthy();
    expect(screen.queryByText(/superseded/i)).toBeNull();
  });
});
