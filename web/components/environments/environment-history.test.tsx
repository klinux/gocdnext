import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { EnvironmentHistory } from "./environment-history.client";
import type { DeploymentsList } from "@/types/api";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("EnvironmentHistory", () => {
  const initial: DeploymentsList = {
    total: 3,
    next_cursor: "cursor-1",
    environment: {
      id: "env-1",
      name: "production",
      total_deploys: 3,
      current_revision_id: "rev-2",
    },
    deployments: [
      {
        id: "rev-2",
        run_id: "run-2",
        attempt: 0,
        version: "1.2.0",
        status: "success",
        is_rollback: false,
        deployed_by: "alice",
        created_at: "2026-06-13T12:00:00Z",
        finished_at: "2026-06-13T12:03:00Z",
      },
      {
        id: "rev-1",
        run_id: "run-1",
        attempt: 0,
        version: "1.1.0",
        status: "failed",
        is_rollback: false,
        created_at: "2026-06-13T11:00:00Z",
        finished_at: "2026-06-13T11:03:00Z",
      },
    ],
  };

  it("loads the next cursor page and appends rows", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async (): Promise<DeploymentsList> => ({
        total: 3,
        deployments: [
          {
            id: "rev-0",
            attempt: 0,
            version: "1.0.0",
            status: "success",
            is_rollback: true,
            created_at: "2026-06-13T10:00:00Z",
            finished_at: "2026-06-13T10:03:00Z",
          },
        ],
      }),
    });
    vi.stubGlobal("fetch", fetchMock);

    render(
      <EnvironmentHistory
        slug="acme"
        environmentId="env-1"
        environmentName="production"
        currentRevisionId="rev-2"
        initial={initial}
        apiBaseURL=""
        canManage={false}
      />,
    );

    expect(screen.getByText("current")).toBeTruthy();
    expect(screen.getByText("Showing 2 of 3 deployments")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Load more" }));

    await waitFor(() => expect(screen.getByText("1.0.0")).toBeTruthy());
    expect(screen.getByText("Showing 3 of 3 deployments")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toContain(
      "/api/v1/projects/acme/environments/env-1/deployments?limit=50&cursor=cursor-1",
    );
  });
});
