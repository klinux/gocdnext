import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";

import { EnvironmentsExplorer } from "./environments-explorer.client";
import type { DeployTarget, EnvironmentSummary } from "@/types/api";

vi.mock("./deploy-watches-provider.client", () => ({
  DeployWatchesProvider: ({ children }: { children: ReactNode }) => (
    <div data-testid="deploy-watches-provider">{children}</div>
  ),
}));

vi.mock("./environment-card.client", async () => {
  const React = await vi.importActual<typeof import("react")>("react");

  const EnvironmentCard = React.forwardRef<
    { setHistoryOpen: (open: boolean) => void },
    {
      environment: EnvironmentSummary;
      deployTarget?: DeployTarget;
      onHistoryOpenChange?: (open: boolean) => void;
    }
  >(function MockEnvironmentCard(
    { environment, deployTarget, onHistoryOpenChange },
    ref,
  ) {
    const [historyOpen, setHistoryOpen] = React.useState(false);
    const setOpen = React.useCallback(
      (open: boolean) => {
        setHistoryOpen(open);
        onHistoryOpenChange?.(open);
      },
      [onHistoryOpenChange],
    );

    React.useImperativeHandle(
      ref,
      () => ({
        setHistoryOpen: setOpen,
      }),
      [setOpen],
    );

    return (
      <article data-testid={`env-card-${environment.name}`}>
        <h3>{environment.name}</h3>
        <p>{environment.current?.version ?? "no deploy"}</p>
        {deployTarget ? <p>target: {deployTarget.application}</p> : null}
        {historyOpen ? (
          <p data-testid={`history-open-${environment.name}`}>history open</p>
        ) : null}
        <button type="button" onClick={() => setOpen(!historyOpen)}>
          Toggle history for {environment.name}
        </button>
      </article>
    );
  });

  return { EnvironmentCard };
});

const environments: EnvironmentSummary[] = [
  {
    id: "env-production",
    name: "production",
    has_environment_row: true,
    frozen: false,
    current: {
      id: "rev-production",
      run_id: "run-1",
      attempt: 0,
      version: "1.42.abc123",
      status: "success",
      is_rollback: false,
      created_at: "2026-06-13T09:58:00Z",
      finished_at: "2026-06-13T10:00:00Z",
    },
  },
  {
    id: "env-canary",
    name: "canary",
    has_environment_row: true,
    frozen: true,
    current: {
      id: "rev-canary",
      run_id: "run-2",
      attempt: 0,
      version: "2.0.0",
      status: "failed",
      is_rollback: false,
      created_at: "2026-06-13T09:58:00Z",
      finished_at: "2026-06-13T10:00:00Z",
    },
  },
  {
    id: "env-preview",
    name: "preview",
    has_environment_row: true,
    frozen: false,
    current: null,
  },
  {
    name: "held-before-first-deploy",
    has_environment_row: false,
    frozen: true,
    current: null,
  },
];

const targets: DeployTarget[] = [
  {
    environment: "production",
    provider: "argocd",
    cluster: "prod-hub",
    application: "checkout-prod",
    namespace: "argocd",
    sync_mode: "trigger",
  },
];

function renderExplorer() {
  render(
    <EnvironmentsExplorer
      slug="acme"
      environments={environments}
      targets={targets}
      canManage
      isAdmin={false}
      apiBaseURL=""
    />,
  );
}

describe("EnvironmentsExplorer", () => {
  it("filters by environment name, current version, or target and updates the counter", () => {
    renderExplorer();
    expect(screen.getByText("4 of 4 environments")).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Search environments"), {
      target: { value: "1.42" },
    });

    expect(screen.getByTestId("env-card-production")).toBeTruthy();
    expect(screen.queryByTestId("env-card-canary")).toBeNull();
    expect(screen.queryByTestId("env-card-preview")).toBeNull();
    expect(screen.getByText("1 of 4 environments")).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Search environments"), {
      target: { value: "prod-hub" },
    });

    expect(screen.getByTestId("env-card-production")).toBeTruthy();
    expect(screen.queryByTestId("env-card-canary")).toBeNull();
    expect(screen.getByText("1 of 4 environments")).toBeTruthy();
  });

  it("segments active, frozen, and no-deploy environments", () => {
    renderExplorer();

    fireEvent.click(screen.getByRole("button", { name: "Frozen (2)" }));
    expect(screen.getByTestId("env-card-canary")).toBeTruthy();
    expect(screen.getByTestId("env-card-held-before-first-deploy")).toBeTruthy();
    expect(screen.queryByTestId("env-card-production")).toBeNull();
    expect(screen.queryByTestId("env-card-preview")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "No deploy (1)" }));
    expect(screen.getByTestId("env-card-preview")).toBeTruthy();
    expect(screen.queryByTestId("env-card-held-before-first-deploy")).toBeNull();
  });

  it("expands and collapses histories for visible environments with rows", () => {
    renderExplorer();

    fireEvent.click(screen.getByRole("button", { name: "Frozen (2)" }));
    fireEvent.click(screen.getByRole("button", { name: "Expand histories" }));
    expect(screen.getByTestId("history-open-canary")).toBeTruthy();
    expect(
      screen.queryByTestId("history-open-held-before-first-deploy"),
    ).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "All (4)" }));
    expect(screen.queryByTestId("history-open-production")).toBeNull();
    expect(screen.getByTestId("history-open-canary")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Expand histories" }));
    expect(screen.getByTestId("history-open-production")).toBeTruthy();
    expect(screen.getByTestId("history-open-preview")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Collapse histories" }));
    expect(screen.queryByTestId("history-open-production")).toBeNull();
    expect(screen.queryByTestId("history-open-canary")).toBeNull();
  });

  it("shows an empty filtered state", () => {
    renderExplorer();
    fireEvent.change(screen.getByLabelText("Search environments"), {
      target: { value: "does-not-exist" },
    });

    expect(screen.getByText("0 of 4 environments")).toBeTruthy();
    expect(
      screen.getByText("No environments match the current filters."),
    ).toBeTruthy();
  });
});
