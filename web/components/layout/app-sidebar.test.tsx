import { render, screen } from "@testing-library/react";
import { beforeAll, describe, expect, it, vi } from "vitest";

vi.mock("next/navigation", () => ({
  usePathname: () => "/",
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
}));

import { SidebarProvider } from "@/components/ui/sidebar";

import { AppSidebar } from "./app-sidebar.client";

beforeAll(() => {
  // shadcn's useIsMobile reads matchMedia, which jsdom lacks.
  window.matchMedia = vi.fn().mockReturnValue({
    matches: false,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  });
});

function renderSidebar(showAdmin?: boolean) {
  return render(
    <SidebarProvider>
      <AppSidebar showAdmin={showAdmin} />
    </SidebarProvider>,
  );
}

describe("AppSidebar", () => {
  it("always shows the workspace nav", () => {
    renderSidebar(false);
    for (const label of ["Dashboard", "Projects", "Runs", "Plugins"]) {
      expect(screen.getByRole("link", { name: label })).toBeTruthy();
    }
  });

  // Every /admin/* and /settings/* read is admin-only server-side; showing the
  // links to anyone else only leads them into a 403.
  it("hides the admin group by default", () => {
    renderSidebar();
    expect(screen.queryByText("Admin")).toBeNull();
    for (const label of ["Settings", "Users", "Secrets", "Service accounts"]) {
      expect(screen.queryByRole("link", { name: label })).toBeNull();
    }
  });

  it("shows the admin group for an admin", () => {
    renderSidebar(true);
    expect(screen.getByText("Admin")).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "Users" }).getAttribute("href"),
    ).toBe("/admin/users");
  });
});
