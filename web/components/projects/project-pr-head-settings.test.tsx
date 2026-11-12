import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { toast } from "sonner";

import { ProjectPrHeadSettings } from "./project-pr-head-settings.client";

const setProjectPrHeadTrust = vi.fn(async (_i: Record<string, unknown>) => ({
  ok: true as const,
}));

vi.mock("@/server/actions/project-settings", () => ({
  setProjectPrHeadTrust: (i: Record<string, unknown>) =>
    setProjectPrHeadTrust(i),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

describe("ProjectPrHeadSettings", () => {
  beforeEach(() => {
    setProjectPrHeadTrust.mockClear();
    vi.mocked(toast.success).mockClear();
    vi.mocked(toast.error).mockClear();
  });

  it("toggles on and saves the enabled state (admin)", async () => {
    render(
      <ProjectPrHeadSettings slug="demo" initialEnabled={false} canEdit />,
    );

    const toggle = screen.getByRole("switch", {
      name: /run same-repo prs/i,
    });
    const save = screen.getByRole("button", {
      name: /save/i,
    }) as HTMLButtonElement;

    // Save is disabled until the value changes.
    expect(save.disabled).toBe(true);

    fireEvent.click(toggle);
    expect(save.disabled).toBe(false);
    fireEvent.click(save);

    await waitFor(() =>
      expect(setProjectPrHeadTrust).toHaveBeenCalledTimes(1),
    );
    expect(setProjectPrHeadTrust.mock.calls[0]![0]).toEqual({
      slug: "demo",
      enabled: true,
    });
    await waitFor(() => expect(toast.success).toHaveBeenCalledTimes(1));
  });

  it("is read-only for a non-admin", () => {
    render(
      <ProjectPrHeadSettings
        slug="demo"
        initialEnabled={false}
        canEdit={false}
      />,
    );
    const toggle = screen.getByRole("switch", { name: /run same-repo prs/i });
    const save = screen.getByRole("button", {
      name: /save/i,
    }) as HTMLButtonElement;

    // The switch is disabled — clicking it must not flip state or enable Save
    // (a non-admin cannot change the toggle; the server PUT is admin-only too).
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(save.disabled).toBe(true);
    expect(screen.getByText(/requires an admin/i)).toBeTruthy();
  });

  it("surfaces the server error on failure", async () => {
    setProjectPrHeadTrust.mockResolvedValueOnce({
      ok: false as const,
      error: "server 403: changing PR-head config trust requires admin",
    } as never);
    render(
      <ProjectPrHeadSettings slug="demo" initialEnabled={false} canEdit />,
    );

    fireEvent.click(screen.getByRole("switch", { name: /run same-repo prs/i }));
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith(
        "server 403: changing PR-head config trust requires admin",
      ),
    );
  });
});
