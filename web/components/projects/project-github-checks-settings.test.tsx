import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { toast } from "sonner";

import { ProjectGithubChecksSettings } from "./project-github-checks-settings.client";
import { selectOption } from "@/test/select";

const setProjectCheckReporting = vi.fn(async (_i: Record<string, unknown>) => ({
  ok: true as const,
}));

vi.mock("@/server/actions/project-settings", () => ({
  setProjectCheckReporting: (i: Record<string, unknown>) =>
    setProjectCheckReporting(i),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

describe("ProjectGithubChecksSettings", () => {
  beforeEach(() => {
    setProjectCheckReporting.mockClear();
    vi.mocked(toast.success).mockClear();
    vi.mocked(toast.error).mockClear();
  });

  it("renders the current mode and disables Save until changed", () => {
    render(<ProjectGithubChecksSettings slug="demo" initialMode="both" />);
    const save = screen.getByRole("button", {
      name: /save/i,
    }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    // The trigger shows the friendly label, not the raw value.
    expect(
      screen.getByText(/Both — Check Run \+ Commit Status/),
    ).toBeTruthy();
  });

  it("warns about the Required status check when switching to check_run", async () => {
    const user = userEvent.setup();
    render(<ProjectGithubChecksSettings slug="demo" initialMode="both" />);

    await selectOption(
      user,
      screen.getByLabelText("GitHub check reporting mode"),
      "Check Run only",
    );

    // The branch-protection heads-up must surface (project-qualified context).
    expect(screen.getByText(/ci\/gocdnext\/demo\//)).toBeTruthy();
    expect(screen.getByText(/Required/)).toBeTruthy();
  });

  it("saves the selected mode and toasts on success", async () => {
    const user = userEvent.setup();
    render(<ProjectGithubChecksSettings slug="demo" initialMode="both" />);

    await selectOption(
      user,
      screen.getByLabelText("GitHub check reporting mode"),
      "Commit Status only",
    );

    const save = screen.getByRole("button", {
      name: /save/i,
    }) as HTMLButtonElement;
    expect(save.disabled).toBe(false);
    fireEvent.click(save);

    await waitFor(() =>
      expect(setProjectCheckReporting).toHaveBeenCalledTimes(1),
    );
    expect(setProjectCheckReporting.mock.calls[0]![0]).toEqual({
      slug: "demo",
      mode: "commit_status",
    });
    await waitFor(() => expect(toast.success).toHaveBeenCalledTimes(1));
  });

  it("surfaces the server error on failure", async () => {
    setProjectCheckReporting.mockResolvedValueOnce({
      ok: false as const,
      error: "server 404: project not found",
    } as never);
    const user = userEvent.setup();
    render(<ProjectGithubChecksSettings slug="demo" initialMode="both" />);

    await selectOption(
      user,
      screen.getByLabelText("GitHub check reporting mode"),
      "Check Run only",
    );
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith("server 404: project not found"),
    );
  });
});
