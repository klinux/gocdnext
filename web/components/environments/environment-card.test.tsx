import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { EnvironmentCard } from "./environment-card.client";
import type { DeployTarget, EnvironmentSummary } from "@/types/api";

// The history rows mount RollbackButton, which calls useRouter and
// imports the rollback server action — stub both so the card tests
// stay focused on the card's own behaviour.
vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn() }),
}));
vi.mock("@/server/actions/environments", () => ({
  rollbackEnvironment: vi.fn(),
}));

const withCurrent: EnvironmentSummary = {
  id: "env-1",
  name: "production",
  created_at: "2026-06-13T09:00:00Z",
  updated_at: "2026-06-13T10:00:00Z",
  current: {
    id: "rev-1",
    run_id: "run-9",
    attempt: 0,
    version: "1.42.abc123",
    status: "success",
    is_rollback: false,
    deployed_by: "alice",
    created_at: "2026-06-13T09:58:00Z",
    finished_at: "2026-06-13T10:00:00Z",
  },
};

afterEach(() => {
  vi.restoreAllMocks();
});

describe("EnvironmentCard", () => {
  it("shows the current version, deployer and a run link", () => {
    render(<EnvironmentCard slug="acme" environment={withCurrent} apiBaseURL="" />);
    expect(screen.getByText("1.42.abc123")).toBeTruthy();
    expect(screen.getByText(/by alice/)).toBeTruthy();
    const runLink = screen.getByRole("link", { name: "run" });
    expect(runLink.getAttribute("href")).toBe("/runs/run-9");
  });

  it("renders the empty state when nothing has deployed", () => {
    const empty: EnvironmentSummary = { ...withCurrent, current: null };
    render(<EnvironmentCard slug="acme" environment={empty} apiBaseURL="" />);
    expect(screen.getByText("no deploys yet")).toBeTruthy();
    expect(screen.getByText(/Nothing has shipped/)).toBeTruthy();
  });

  it("flags a rollback deploy", () => {
    const rolled: EnvironmentSummary = {
      ...withCurrent,
      current: { ...withCurrent.current!, is_rollback: true },
    };
    render(<EnvironmentCard slug="acme" environment={rolled} apiBaseURL="" />);
    expect(screen.getAllByText("rollback").length).toBeGreaterThan(0);
  });

  const target: DeployTarget = {
    environment: "production",
    provider: "argocd",
    cluster: "prod-gke",
    application: "checkout",
    namespace: "argocd",
    sync_mode: "trigger",
  };

  it("shows the native provider target when one is registered (maintainer)", () => {
    render(
      <EnvironmentCard
        slug="acme"
        environment={withCurrent}
        deployTarget={target}
        apiBaseURL=""
      />,
    );
    expect(screen.getByText("Native")).toBeTruthy();
    expect(screen.getByText("ArgoCD")).toBeTruthy();
    expect(screen.getByText("checkout")).toBeTruthy();
    expect(screen.getByText("prod-gke")).toBeTruthy();
    expect(screen.getByText("trigger")).toBeTruthy();
  });

  it("omits the native row when there is no target (or the viewer can't see it)", () => {
    render(<EnvironmentCard slug="acme" environment={withCurrent} apiBaseURL="" />);
    expect(screen.queryByText("Native")).toBeNull();
    expect(screen.queryByText("checkout")).toBeNull();
  });

  it("lazily fetches history on expand and lists past deploys", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        deployments: [
          {
            id: "rev-1",
            run_id: "run-9",
            attempt: 0,
            version: "1.42.abc123",
            status: "success",
            is_rollback: false,
            deployed_by: "alice",
            created_at: "2026-06-13T09:58:00Z",
            finished_at: "2026-06-13T10:00:00Z",
          },
          {
            id: "rev-0",
            run_id: "run-8",
            attempt: 0,
            version: "1.41.def456",
            status: "failed",
            is_rollback: false,
            created_at: "2026-06-12T10:00:00Z",
            finished_at: "2026-06-12T10:02:00Z",
          },
        ],
      }),
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<EnvironmentCard slug="acme" environment={withCurrent} apiBaseURL="" />);
    // History is NOT fetched until the operator expands it.
    expect(fetchMock).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: /History/ }));

    await waitFor(() => expect(screen.getByText("1.41.def456")).toBeTruthy());
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toContain(
      "/api/v1/projects/acme/environments/env-1/deployments",
    );
  });

  it("surfaces a history fetch error without crashing", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 500 }));
    render(<EnvironmentCard slug="acme" environment={withCurrent} apiBaseURL="" />);
    fireEvent.click(screen.getByRole("button", { name: /History/ }));
    await waitFor(() => expect(screen.getByText(/Couldn't load history/)).toBeTruthy());
  });
});
