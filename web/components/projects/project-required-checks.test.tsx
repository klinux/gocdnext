import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { toast } from "sonner";

import { ProjectRequiredChecks } from "./project-required-checks.client";
import type { RequiredChecksSettings } from "@/server/queries/projects";

const setProjectRequiredChecks = vi.fn(
  async (_i: Record<string, unknown>) => ({ ok: true as const }),
);

vi.mock("@/server/actions/project-settings", () => ({
  setProjectRequiredChecks: (i: Record<string, unknown>) =>
    setProjectRequiredChecks(i),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

function settings(
  over: Partial<RequiredChecksSettings> = {},
): RequiredChecksSettings {
  return {
    pipelines: [],
    available_pipelines: ["build", "e2e"],
    provider: "github",
    status_contexts: [],
    sync: { status: "not_configured" },
    ...over,
  };
}

describe("ProjectRequiredChecks", () => {
  beforeEach(() => {
    setProjectRequiredChecks.mockClear();
    vi.mocked(toast.success).mockClear();
    vi.mocked(toast.error).mockClear();
  });

  it("lists only the PR-firing pipelines with their contexts", () => {
    render(<ProjectRequiredChecks slug="demo" initial={settings()} />);
    expect(screen.getByText("build")).toBeTruthy();
    expect(screen.getByText("e2e")).toBeTruthy();
    expect(screen.getByText("ci/gocdnext/demo/build")).toBeTruthy();
    // Save disabled until a change is made.
    const save = screen.getByRole("button", {
      name: /save/i,
    }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
  });

  it("toggling a pipeline and saving calls the action with the selection", async () => {
    render(<ProjectRequiredChecks slug="demo" initial={settings()} />);
    fireEvent.click(
      screen.getByRole("switch", { name: /Require build to merge/i }),
    );
    const save = screen.getByRole("button", {
      name: /save/i,
    }) as HTMLButtonElement;
    expect(save.disabled).toBe(false);
    fireEvent.click(save);

    await waitFor(() =>
      expect(setProjectRequiredChecks).toHaveBeenCalledTimes(1),
    );
    expect(setProjectRequiredChecks.mock.calls[0]![0]).toEqual({
      slug: "demo",
      pipelines: ["build"],
    });
    await waitFor(() => expect(toast.success).toHaveBeenCalledTimes(1));
  });

  it("shows a guidance message and disables Save when no PR pipelines exist", () => {
    render(
      <ProjectRequiredChecks
        slug="demo"
        initial={settings({ available_pipelines: [] })}
      />,
    );
    expect(screen.getByText(/No pull-request pipelines/i)).toBeTruthy();
    const save = screen.getByRole("button", {
      name: /save/i,
    }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
  });

  it("surfaces the missing-admin hint and a Retry button on a failed sync", () => {
    render(
      <ProjectRequiredChecks
        slug="demo"
        initial={settings({
          pipelines: ["build"],
          sync: { status: "failed", needs_admin: true, error: "403" },
        })}
      />,
    );
    expect(screen.getByText(/Administration: write/i)).toBeTruthy();
    // Retry must be enabled even with no selection change (the operator just
    // granted the App permission and wants to re-push the same config).
    const retry = screen.getByRole("button", {
      name: /retry/i,
    }) as HTMLButtonElement;
    expect(retry.disabled).toBe(false);
    expect(screen.getByText(/Sync failed/i)).toBeTruthy();
  });
});
