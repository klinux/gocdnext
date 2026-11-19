import { fireEvent, render, screen } from "@testing-library/react";
import { beforeAll, describe, expect, it, vi } from "vitest";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn() }),
}));
vi.mock("next-themes", () => ({
  useTheme: () => ({ setTheme: vi.fn() }),
}));

import { CommandPalette } from "./command-palette.client";

beforeAll(() => {
  // cmdk measures its list and scrolls the active item; jsdom has neither.
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
  Element.prototype.scrollIntoView = vi.fn();
});

function openPalette(showAdmin?: boolean) {
  render(<CommandPalette showAdmin={showAdmin} />);
  fireEvent.keyDown(window, { key: "k", ctrlKey: true });
}

describe("CommandPalette", () => {
  it("lists workspace destinations", () => {
    openPalette(false);
    expect(screen.getByText("Projects")).toBeTruthy();
  });

  it("hides admin destinations by default", () => {
    openPalette();
    for (const label of ["Settings", "Users", "Audit log", "Global secrets"]) {
      expect(screen.queryByText(label)).toBeNull();
    }
  });

  it("shows admin destinations for an admin", () => {
    openPalette(true);
    for (const label of ["Settings", "Users", "Audit log", "Global secrets"]) {
      expect(screen.getByText(label)).toBeTruthy();
    }
  });
});
