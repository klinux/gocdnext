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

  it("omits the winner link when linkWinner is false (inside a row-link)", () => {
    render(
      <RunStatusBadge
        status="canceled"
        cancelReason="superseded by #5"
        supersededBy="run-5-id"
        linkWinner={false}
      />,
    );
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

describe("RunStatusBadge — held by an environment freeze (#202)", () => {
  it("shows the freeze pill ALONGSIDE the status, not instead of it", () => {
    // Unlike a supersede (terminal — the badge replaces the outcome), a freeze
    // holds a run that is still live: dropping "queued" would lose real state.
    render(<RunStatusBadge status="queued" queueReason="frozen-deploy:production" />);
    expect(screen.getByText("Frozen: production")).toBeTruthy();
    expect(screen.getByText("Queued")).toBeTruthy();
  });

  it("shows it on a running run too — a mixed stage keeps dispatching", () => {
    // A stage with a frozen `production` deploy and a dispatchable `staging`
    // one promotes the run to running while the freeze still owns the reason.
    render(<RunStatusBadge status="running" queueReason="frozen-deploy:production" />);
    expect(screen.getByText("Frozen: production")).toBeTruthy();
  });

  it("never shows it on a terminal run", () => {
    // queue_reason on a finished run is a leftover from when it was waiting;
    // "Frozen: production" next to "success" would be actively misleading.
    render(<RunStatusBadge status="success" queueReason="frozen-deploy:production" />);
    expect(screen.queryByText("Frozen: production")).toBeNull();
  });

  it("ignores an unknown queue reason", () => {
    render(<RunStatusBadge status="queued" queueReason="serial-busy:abc" />);
    expect(screen.queryByText(/serial-busy/)).toBeNull();
    expect(screen.getByText("Queued")).toBeTruthy();
  });
});
