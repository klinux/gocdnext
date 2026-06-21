import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { FrameworksManager } from "./frameworks-manager.client";
import type { ComplianceFramework } from "@/server/queries/admin";

const createFramework = vi.fn(async (_i: Record<string, unknown>) => ({ ok: true as const }));
const updateFramework = vi.fn(async (_i: Record<string, unknown>) => ({ ok: true as const }));
const deleteFramework = vi.fn(async (_id: string) => ({ ok: true as const }));

vi.mock("@/server/actions/compliance", () => ({
  createComplianceFramework: (i: Record<string, unknown>) => createFramework(i),
  updateComplianceFramework: (i: Record<string, unknown>) => updateFramework(i),
  deleteComplianceFramework: (id: string) => deleteFramework(id),
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const sample: ComplianceFramework[] = [
  { id: "f1", name: "SOC2", description: "soc2", created_by: "", created_at: "", updated_at: "" },
  { id: "f2", name: "PCI", description: "card data", created_by: "", created_at: "", updated_at: "" },
];

describe("FrameworksManager", () => {
  it("renders a row per framework", () => {
    render(<FrameworksManager frameworks={sample} />);
    expect(screen.getByText("SOC2")).toBeTruthy();
    expect(screen.getByText("card data")).toBeTruthy();
  });

  it("shows the empty state", () => {
    render(<FrameworksManager frameworks={[]} />);
    expect(screen.getByText(/No frameworks yet/i)).toBeTruthy();
  });

  it("creates a framework", async () => {
    render(<FrameworksManager frameworks={[]} />);
    fireEvent.click(screen.getByRole("button", { name: /new framework/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "HIPAA" } });
    fireEvent.change(screen.getByLabelText("Description"), {
      target: { value: "health" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() =>
      expect(createFramework).toHaveBeenCalledWith({ name: "HIPAA", description: "health" }),
    );
  });

  it("confirms before deleting", () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    render(<FrameworksManager frameworks={sample} />);
    fireEvent.click(screen.getByRole("button", { name: /delete SOC2/i }));
    expect(confirmSpy).toHaveBeenCalledWith(expect.stringContaining('Delete framework "SOC2"'));
    expect(deleteFramework).not.toHaveBeenCalled();
    confirmSpy.mockRestore();
  });
});
