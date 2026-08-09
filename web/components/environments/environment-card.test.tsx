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
  setDeployTarget: vi.fn(),
  deleteDeployTarget: vi.fn(),
  deleteEnvironment: vi.fn(),
  freezeEnvironment: vi.fn(),
  unfreezeEnvironment: vi.fn(),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const withCurrent: EnvironmentSummary = {
  id: "env-1",
  name: "production",
  has_environment_row: true,
  frozen: false,
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

function openEnvironmentActions(environmentName = "production") {
  fireEvent.click(
    screen.getByRole("button", {
      name: new RegExp(`Environment actions for ${environmentName}`, "i"),
    }),
  );
}

describe("EnvironmentCard", () => {
  it("shows the current version, deployer and a run link", () => {
    render(<EnvironmentCard slug="acme" environment={withCurrent} apiBaseURL="" canManage={false} isAdmin={false} />);
    expect(screen.getByText("1.42.abc123")).toBeTruthy();
    expect(screen.getByText(/by alice/)).toBeTruthy();
    const runLink = screen.getByRole("link", { name: "run" });
    expect(runLink.getAttribute("href")).toBe("/runs/run-9");
  });

  it("renders the empty state when nothing has deployed", () => {
    const empty: EnvironmentSummary = { ...withCurrent, current: null };
    render(<EnvironmentCard slug="acme" environment={empty} apiBaseURL="" canManage={false} isAdmin={false} />);
    expect(screen.getByText("no deploys yet")).toBeTruthy();
    expect(screen.getByText(/Nothing has shipped/)).toBeTruthy();
  });

  it("flags a rollback deploy", () => {
    const rolled: EnvironmentSummary = {
      ...withCurrent,
      current: { ...withCurrent.current!, is_rollback: true },
    };
    render(<EnvironmentCard slug="acme" environment={rolled} apiBaseURL="" canManage={false} isAdmin={false} />);
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
        canManage={false}
        isAdmin={false}
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
    render(<EnvironmentCard slug="acme" environment={withCurrent} apiBaseURL="" canManage={false} isAdmin={false} />);
    expect(screen.queryByText("Native")).toBeNull();
    expect(screen.queryByText("checkout")).toBeNull();
  });

  it("offers an Edit affordance in the actions menu for managers", async () => {
    render(
      <EnvironmentCard
        slug="acme"
        environment={withCurrent}
        deployTarget={target}
        canManage
        isAdmin={false}
        apiBaseURL=""
      />,
    );
    openEnvironmentActions();
    fireEvent.click(screen.getByRole("menuitem", { name: /Edit native target/i }));
    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.getByText("Edit native deploy target")).toBeTruthy();
  });

  it("offers 'Add native target' in the actions menu on a target-less env for managers", async () => {
    render(
      <EnvironmentCard
        slug="acme"
        environment={withCurrent}
        canManage
        isAdmin={false}
        apiBaseURL=""
      />,
    );
    openEnvironmentActions();
    fireEvent.click(screen.getByRole("menuitem", { name: /Add native target/i }));
    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.getByText("Register native deploy target")).toBeTruthy();
  });

  it("hides management affordances from non-managers", () => {
    render(
      <EnvironmentCard
        slug="acme"
        environment={withCurrent}
        deployTarget={target}
        canManage={false}
        isAdmin={false}
        apiBaseURL=""
      />,
    );
    expect(
      screen.queryByRole("button", { name: /Environment actions/i }),
    ).toBeNull();
    expect(screen.queryByRole("menuitem", { name: /Edit native target/i })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: /Add native target/i })).toBeNull();
  });

  it("offers Remove only to admins, with a confirm step", () => {
    const { unmount } = render(
      <EnvironmentCard
        slug="acme"
        environment={withCurrent}
        canManage
        isAdmin={false}
        apiBaseURL=""
      />,
    );
    // A maintainer (canManage but not admin) never sees Remove.
    openEnvironmentActions();
    expect(
      screen.queryByRole("menuitem", { name: /Remove environment/i }),
    ).toBeNull();
    unmount();

    render(
      <EnvironmentCard
        slug="acme"
        environment={withCurrent}
        canManage
        isAdmin
        apiBaseURL=""
      />,
    );
    // Admin sees Remove; it's a two-step confirm (no immediate delete).
    openEnvironmentActions();
    fireEvent.click(screen.getByRole("menuitem", { name: /Remove environment/i }));
    expect(screen.getByRole("button", { name: "Confirm" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
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

    render(<EnvironmentCard slug="acme" environment={withCurrent} apiBaseURL="" canManage={false} isAdmin={false} />);
    // History is NOT fetched until the operator expands it.
    expect(fetchMock).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: /History/ }));

    await waitFor(() => expect(screen.getByText("1.41.def456")).toBeTruthy());
    const region = screen.getByRole("region", {
      name: /production deployment history/i,
    });
    const scroll = region.querySelector("[data-history-scroll]");
    expect(scroll?.className).toContain("max-h-[196px]");
    expect(scroll?.className).toContain("overflow-y-auto");
    expect(screen.getByText("current")).toBeTruthy();
    expect(screen.getByText("Showing 2 latest deployments")).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toContain(
      "/api/v1/projects/acme/environments/env-1/deployments",
    );
  });

  it("surfaces a history fetch error and lets the operator retry", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: false, status: 500 })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          deployments: [
            {
              id: "rev-retry",
              run_id: "run-retry",
              attempt: 0,
              version: "1.40.retry",
              status: "success",
              is_rollback: false,
              created_at: "2026-06-12T09:58:00Z",
              finished_at: "2026-06-12T10:00:00Z",
            },
          ],
        }),
      });
    vi.stubGlobal("fetch", fetchMock);
    render(<EnvironmentCard slug="acme" environment={withCurrent} apiBaseURL="" canManage={false} isAdmin={false} />);
    fireEvent.click(screen.getByRole("button", { name: /History/ }));
    await waitFor(() => expect(screen.getByText(/Couldn't load history/)).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByText("1.40.retry")).toBeTruthy());
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});

// ---- environment change-freeze (#202) -------------------------------------

describe("EnvironmentCard — change-freeze", () => {
  const frozen: EnvironmentSummary = {
    ...withCurrent,
    frozen: true,
    frozen_at: "2026-07-01T08:00:00Z",
    frozen_by: "alice@acme.io",
    freeze_reason: "month-end close",
  };

  it("banners a frozen environment with its reason and who froze it", () => {
    render(
      <EnvironmentCard
        slug="acme"
        environment={frozen}
        canManage
        isAdmin={false}
        apiBaseURL=""
      />,
    );
    expect(screen.getByText(/no deploy to this environment will be admitted/i)).toBeTruthy();
    expect(screen.getByText("month-end close")).toBeTruthy();
    expect(screen.getByText(/by alice@acme.io/)).toBeTruthy();
  });

  it("still states the freeze for a viewer whose reason/actor were redacted", () => {
    // The server redacts freeze_reason + frozen_by below maintainer, so their
    // absence must not degrade into a card that looks un-frozen.
    const redacted: EnvironmentSummary = {
      ...frozen,
      frozen_by: undefined,
      freeze_reason: undefined,
    };
    render(
      <EnvironmentCard
        slug="acme"
        environment={redacted}
        canManage={false}
        isAdmin={false}
        apiBaseURL=""
      />,
    );
    expect(screen.getByText(/no deploy to this environment will be admitted/i)).toBeTruthy();
    expect(screen.queryByText("month-end close")).toBeNull();
  });

  it("offers Freeze/Unfreeze only to managers", () => {
    const { rerender } = render(
      <EnvironmentCard
        slug="acme"
        environment={withCurrent}
        canManage={false}
        isAdmin={false}
        apiBaseURL=""
      />,
    );
    expect(screen.queryByRole("button", { name: "Freeze" })).toBeNull();

    rerender(
      <EnvironmentCard
        slug="acme"
        environment={withCurrent}
        canManage
        isAdmin={false}
        apiBaseURL=""
      />,
    );
    expect(screen.getByRole("button", { name: "Freeze" })).toBeTruthy();

    rerender(
      <EnvironmentCard
        slug="acme"
        environment={frozen}
        canManage
        isAdmin={false}
        apiBaseURL=""
      />,
    );
    expect(screen.getByRole("button", { name: "Unfreeze" })).toBeTruthy();
  });

  it("hides history/remove on a freeze-only row and shows frozen_at, never a zero time", () => {
    // An orphan freeze (env deleted, or never deployed to) has no environments
    // row: id/created_at/updated_at are absent, and every id-addressed
    // affordance must disappear rather than build a URL with `undefined`.
    const orphan: EnvironmentSummary = {
      name: "production",
      has_environment_row: false,
      frozen: true,
      frozen_at: "2026-07-01T08:00:00Z",
      current: null,
    };
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    render(
      <EnvironmentCard
        slug="acme"
        environment={orphan}
        canManage
        isAdmin
        apiBaseURL=""
      />,
    );
    expect(screen.queryByRole("button", { name: /History/ })).toBeNull();
    openEnvironmentActions();
    expect(
      screen.queryByRole("menuitem", { name: /Remove environment/i }),
    ).toBeNull();
    // Unfreezing it is still possible — otherwise the freeze would be permanent.
    expect(screen.getByRole("button", { name: "Unfreeze" })).toBeTruthy();
    expect(screen.getByText(/doesn't exist yet/i)).toBeTruthy();
    // No zero-time render leaking through from a missing created_at.
    expect(screen.queryByText(/0001/)).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
