import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { PoliciesManager } from "./policies-manager.client";
import type { ComplianceFramework, CompliancePolicy } from "@/server/queries/admin";

const createPolicy = vi.fn(async (_i: Record<string, unknown>) => ({ ok: true as const }));
const updatePolicy = vi.fn(async (_i: Record<string, unknown>) => ({ ok: true as const }));
const deletePolicy = vi.fn(async (_id: string) => ({ ok: true as const }));

vi.mock("@/server/actions/compliance", () => ({
  createCompliancePolicy: (i: Record<string, unknown>) => createPolicy(i),
  updateCompliancePolicy: (i: Record<string, unknown>) => updatePolicy(i),
  deleteCompliancePolicy: (id: string) => deletePolicy(id),
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const frameworks: ComplianceFramework[] = [
  { id: "f1", name: "SOC2", description: "", created_by: "", created_at: "", updated_at: "" },
  { id: "f2", name: "PCI", description: "", created_by: "", created_at: "", updated_at: "" },
];

const policies: CompliancePolicy[] = [
  {
    id: "p1", name: "pci-scan", description: "scan", enabled: true, mode: "inject",
    priority: 0, applies_to_all: false, position_before: "", position_after: "",
    framework_ids: ["f2"], config_yaml: "stages: [_compliance_scan]",
    created_by: "", created_at: "", updated_at: "",
  },
];

beforeEach(() => {
  createPolicy.mockClear();
  updatePolicy.mockClear();
  deletePolicy.mockClear();
});

describe("PoliciesManager", () => {
  it("renders policies with scope + status", () => {
    render(<PoliciesManager policies={policies} frameworks={frameworks} />);
    expect(screen.getByText("pci-scan")).toBeTruthy();
    expect(screen.getByText("enabled")).toBeTruthy();
    // Framework id resolved to its name in the scope column.
    expect(screen.getByText("PCI")).toBeTruthy();
  });

  it("shows the empty state", () => {
    render(<PoliciesManager policies={[]} frameworks={frameworks} />);
    expect(screen.getByText(/No policies yet/i)).toBeTruthy();
  });

  it("creates a policy with a selected framework and config", async () => {
    const user = userEvent.setup();
    render(<PoliciesManager policies={[]} frameworks={frameworks} />);
    fireEvent.click(screen.getByRole("button", { name: /new policy/i }));

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "global-scan" } });
    fireEvent.change(screen.getByLabelText(/Policy config/i), {
      target: { value: "stages: [_compliance_scan]" },
    });
    // Select the SOC2 framework (badge toggle).
    await user.click(screen.getByRole("button", { name: "Framework SOC2" }));

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => expect(createPolicy).toHaveBeenCalledTimes(1));
    const arg = createPolicy.mock.calls[0]![0];
    expect(arg.name).toBe("global-scan");
    expect(arg.mode).toBe("inject");
    expect(arg.framework_ids).toEqual(["f1"]);
    expect(arg.config_yaml).toContain("_compliance_scan");
  });

  it("blocks save when config YAML is empty", () => {
    const { container } = render(
      <PoliciesManager policies={[]} frameworks={frameworks} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /new policy/i }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    expect(createPolicy).not.toHaveBeenCalled();
    void container;
  });
});
