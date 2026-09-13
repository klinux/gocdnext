import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { toast } from "sonner";

import { ProjectStatusBadgeSettings } from "./project-status-badge-settings.client";

const rotateProjectBadgeToken = vi.fn(async (_i: Record<string, unknown>) => ({
  ok: true as const,
  enabled: true as const,
  token: "badge-token",
  badge_url: "/api/v1/badge/demo.svg?token=badge-token",
  markdown: "![build](/api/v1/badge/demo.svg?token=badge-token)",
}));
const disableProjectBadgeToken = vi.fn(async (_i: Record<string, unknown>) => ({
  ok: true as const,
}));

vi.mock("@/server/actions/project-settings", () => ({
  rotateProjectBadgeToken: (i: Record<string, unknown>) =>
    rotateProjectBadgeToken(i),
  disableProjectBadgeToken: (i: Record<string, unknown>) =>
    disableProjectBadgeToken(i),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

describe("ProjectStatusBadgeSettings", () => {
  const writeText = vi.fn(async (_text: string) => undefined);

  beforeEach(() => {
    rotateProjectBadgeToken.mockClear();
    disableProjectBadgeToken.mockClear();
    writeText.mockClear();
    vi.mocked(toast.success).mockClear();
    vi.mocked(toast.error).mockClear();
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
  });

  it("generates a token and shows the Markdown exactly once", async () => {
    render(
      <ProjectStatusBadgeSettings slug="demo" initialEnabled={false} />,
    );

    expect(screen.getByText("Disabled")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /generate badge token/i }));

    await waitFor(() =>
      expect(rotateProjectBadgeToken).toHaveBeenCalledTimes(1),
    );
    expect(rotateProjectBadgeToken.mock.calls[0]![0]).toEqual({
      slug: "demo",
    });
    expect(screen.getByText("Enabled")).toBeTruthy();
    expect(
      (screen.getByLabelText("Badge Markdown") as HTMLTextAreaElement).value,
    ).toBe("![build](/api/v1/badge/demo.svg?token=badge-token)");

    fireEvent.click(screen.getByRole("button", { name: /copy markdown/i }));
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith(
        "![build](/api/v1/badge/demo.svg?token=badge-token)",
      ),
    );

    fireEvent.click(screen.getByRole("button", { name: /copy link/i }));
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith(
        "/api/v1/badge/demo.svg?token=badge-token",
      ),
    );
  });

  it("starts enabled without exposing an old token", () => {
    render(<ProjectStatusBadgeSettings slug="demo" initialEnabled />);

    expect(screen.getByText("Enabled")).toBeTruthy();
    expect(screen.getByText(/plaintext token is not stored/i)).toBeTruthy();
    expect(screen.queryByLabelText("Badge Markdown")).toBeNull();
    expect(
      screen.getByRole("button", { name: /rotate token/i }),
    ).toBeTruthy();
  });

  it("disables the badge and clears the generated Markdown", async () => {
    render(<ProjectStatusBadgeSettings slug="demo" initialEnabled />);

    fireEvent.click(screen.getByRole("button", { name: /disable badge/i }));

    await waitFor(() =>
      expect(disableProjectBadgeToken).toHaveBeenCalledTimes(1),
    );
    expect(disableProjectBadgeToken.mock.calls[0]![0]).toEqual({
      slug: "demo",
    });
    expect(screen.getByText("Disabled")).toBeTruthy();
    expect(screen.queryByLabelText("Badge Markdown")).toBeNull();
  });

  it("surfaces rotate failures", async () => {
    rotateProjectBadgeToken.mockResolvedValueOnce({
      ok: false as const,
      error: "server 500: nope",
    } as never);
    render(
      <ProjectStatusBadgeSettings slug="demo" initialEnabled={false} />,
    );

    fireEvent.click(screen.getByRole("button", { name: /generate badge token/i }));

    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith("server 500: nope"),
    );
    expect(screen.getByText("Disabled")).toBeTruthy();
  });
});
