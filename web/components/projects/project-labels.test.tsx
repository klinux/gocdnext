import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { toast } from "sonner";

import { ProjectLabelsCard } from "./project-labels.client";
import type { ProjectLabel } from "@/types/api";

const setProjectLabels = vi.fn(async (_i: Record<string, unknown>) => ({ ok: true as const }));

vi.mock("@/server/actions/project-settings", () => ({
  setProjectLabels: (i: Record<string, unknown>) => setProjectLabels(i),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const initial: ProjectLabel[] = [{ key: "team", value: "payments" }];

describe("ProjectLabelsCard", () => {
  beforeEach(() => {
    setProjectLabels.mockClear();
    vi.mocked(toast.error).mockClear();
  });

  it("renders existing labels", () => {
    render(<ProjectLabelsCard slug="demo" initial={initial} />);
    expect((screen.getByLabelText("Label 1 key") as HTMLInputElement).value).toBe("team");
    expect((screen.getByLabelText("Label 1 value") as HTMLInputElement).value).toBe("payments");
  });

  it("adds a label, drops empty-key rows, trims, and saves the full set", async () => {
    render(<ProjectLabelsCard slug="demo" initial={initial} />);

    // Add a second row → fill it (with surrounding whitespace).
    fireEvent.click(screen.getByRole("button", { name: /add label/i }));
    fireEvent.change(screen.getByLabelText("Label 2 key"), { target: { value: " tier " } });
    fireEvent.change(screen.getByLabelText("Label 2 value"), { target: { value: " critical " } });

    // Add a third, empty row → must be dropped on save.
    fireEvent.click(screen.getByRole("button", { name: /add label/i }));

    fireEvent.click(screen.getByRole("button", { name: /save labels/i }));

    await waitFor(() => expect(setProjectLabels).toHaveBeenCalledTimes(1));
    expect(setProjectLabels.mock.calls[0]![0]).toEqual({
      slug: "demo",
      labels: [
        { key: "team", value: "payments" },
        { key: "tier", value: "critical" },
      ],
    });
  });

  it("does not silently drop a value-only row", () => {
    render(<ProjectLabelsCard slug="demo" initial={initial} />);

    fireEvent.click(screen.getByRole("button", { name: /add label/i }));
    fireEvent.change(screen.getByLabelText("Label 2 value"), { target: { value: "payments" } });
    fireEvent.click(screen.getByRole("button", { name: /save labels/i }));

    expect(setProjectLabels).not.toHaveBeenCalled();
    expect(toast.error).toHaveBeenCalledWith("Label key is required");
  });
});
