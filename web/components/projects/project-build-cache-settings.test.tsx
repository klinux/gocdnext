import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { toast } from "sonner";

import { ProjectBuildCacheSettings } from "./project-build-cache-settings.client";

const rotateProjectCacheToken = vi.fn(async (_i: Record<string, unknown>) => ({
  ok: true as const,
  enabled: true as const,
  token: "cache-token",
  cache_url: "https://ci.example.com/api/v1/buildcache/demo/",
  gradle_snippet: "buildCache { /* demo */ }",
}));
const disableProjectCacheToken = vi.fn(async (_i: Record<string, unknown>) => ({
  ok: true as const,
}));

vi.mock("@/server/actions/project-settings", () => ({
  rotateProjectCacheToken: (i: Record<string, unknown>) =>
    rotateProjectCacheToken(i),
  disableProjectCacheToken: (i: Record<string, unknown>) =>
    disableProjectCacheToken(i),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

describe("ProjectBuildCacheSettings", () => {
  const writeText = vi.fn(async (_text: string) => undefined);

  beforeEach(() => {
    rotateProjectCacheToken.mockClear();
    disableProjectCacheToken.mockClear();
    writeText.mockClear();
    vi.mocked(toast.success).mockClear();
    vi.mocked(toast.error).mockClear();
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
  });

  it("enables the cache and shows the snippet + token once", async () => {
    render(<ProjectBuildCacheSettings slug="demo" initialEnabled={false} />);

    expect(screen.getByText("Disabled")).toBeTruthy();
    fireEvent.click(
      screen.getByRole("button", { name: /enable build cache/i }),
    );

    await waitFor(() =>
      expect(rotateProjectCacheToken).toHaveBeenCalledTimes(1),
    );
    expect(rotateProjectCacheToken.mock.calls[0]![0]).toEqual({ slug: "demo" });
    expect(screen.getByText("Enabled")).toBeTruthy();
    // The token is shown once and is copyable.
    expect(
      (screen.getByLabelText("Build cache token") as HTMLInputElement).value,
    ).toBe("cache-token");
    fireEvent.click(screen.getByRole("button", { name: /copy token/i }));
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith("cache-token"),
    );

    expect(
      (
        screen.getByLabelText(
          "Gradle buildCache snippet",
        ) as HTMLTextAreaElement
      ).value,
    ).toContain("buildCache");

    fireEvent.click(screen.getByRole("button", { name: /copy snippet/i }));
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith("buildCache { /* demo */ }"),
    );
    fireEvent.click(screen.getByRole("button", { name: /copy url/i }));
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith(
        "https://ci.example.com/api/v1/buildcache/demo/",
      ),
    );
  });

  it("starts enabled without exposing an old token", () => {
    render(<ProjectBuildCacheSettings slug="demo" initialEnabled />);
    expect(screen.getByText("Enabled")).toBeTruthy();
    expect(screen.getByText(/token is not stored in the UI/i)).toBeTruthy();
    expect(screen.queryByLabelText("Gradle buildCache snippet")).toBeNull();
    expect(screen.getByRole("button", { name: /rotate token/i })).toBeTruthy();
  });

  it("disables the cache and clears the snippet", async () => {
    render(<ProjectBuildCacheSettings slug="demo" initialEnabled />);
    fireEvent.click(screen.getByRole("button", { name: /disable/i }));
    await waitFor(() =>
      expect(disableProjectCacheToken).toHaveBeenCalledTimes(1),
    );
    expect(screen.getByText("Disabled")).toBeTruthy();
    expect(screen.queryByLabelText("Gradle buildCache snippet")).toBeNull();
  });

  it("surfaces rotate failures", async () => {
    rotateProjectCacheToken.mockResolvedValueOnce({
      ok: false as const,
      error: "server 500: nope",
    } as never);
    render(<ProjectBuildCacheSettings slug="demo" initialEnabled={false} />);
    fireEvent.click(
      screen.getByRole("button", { name: /enable build cache/i }),
    );
    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith("server 500: nope"),
    );
    expect(screen.getByText("Disabled")).toBeTruthy();
  });
});
