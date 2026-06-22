import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { ProjectCompliancePreview } from "./project-compliance-preview.client";
import type {
  ComplianceFramework,
  EffectivePipelinePreview,
} from "@/server/queries/admin";

const previewAction = vi.fn(
  async (_i: { slug: string; framework_ids: string[] }) => ({
    ok: true as const,
    data: whatIfResult,
  }),
);

vi.mock("@/server/actions/compliance", () => ({
  previewEffectivePipeline: (i: { slug: string; framework_ids: string[] }) =>
    previewAction(i),
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const frameworks: ComplianceFramework[] = [
  { id: "f1", name: "PCI", description: "", created_by: "", created_at: "", updated_at: "" },
  { id: "f2", name: "SOC2", description: "", created_by: "", created_at: "", updated_at: "" },
];

const stored: EffectivePipelinePreview[] = [
  {
    name: "main",
    system_managed: false,
    raw: { stages: ["build"], jobs: [{ name: "compile", stage: "build" }] },
    effective: {
      stages: ["_compliance_scan", "build"],
      jobs: [
        { name: "_compliance_scan", stage: "_compliance_scan" },
        { name: "compile", stage: "build" },
      ],
    },
  },
];

const whatIfResult: EffectivePipelinePreview[] = [
  {
    name: "_compliance",
    system_managed: true,
    raw: { stages: [], jobs: [] },
    effective: {
      stages: ["_compliance_scan"],
      jobs: [{ name: "_compliance_scan", stage: "_compliance_scan" }],
    },
  },
];

describe("ProjectCompliancePreview", () => {
  it("renders the stored effective definition with enforced entries badged", () => {
    render(
      <ProjectCompliancePreview
        slug="payments"
        frameworks={frameworks}
        assignedIDs={["f1"]}
        initial={stored}
      />,
    );
    expect(screen.getByText("main")).toBeTruthy();
    // The policy-injected job is shown (as a stage badge + a job row) and
    // flagged enforced.
    expect(screen.getAllByText("_compliance_scan").length).toBeGreaterThan(0);
    expect(screen.getAllByText(/enforced/i).length).toBeGreaterThan(0);
    // The repo's own job is shown without an enforced badge.
    expect(screen.getByText("compile")).toBeTruthy();
  });

  it("shows the empty state when there are no pipelines", () => {
    render(
      <ProjectCompliancePreview
        slug="payments"
        frameworks={frameworks}
        assignedIDs={[]}
        initial={[]}
      />,
    );
    expect(screen.getByText(/No pipelines to preview/i)).toBeTruthy();
  });

  it("runs a what-if recompute seeded with the assigned frameworks", async () => {
    render(
      <ProjectCompliancePreview
        slug="payments"
        frameworks={frameworks}
        assignedIDs={["f1"]}
        initial={stored}
      />,
    );
    // Framework chips are hidden until what-if is on.
    expect(screen.queryByRole("button", { name: /Framework PCI/i })).toBeNull();

    fireEvent.click(screen.getByRole("switch"));
    await waitFor(() =>
      expect(previewAction).toHaveBeenCalledWith({
        slug: "payments",
        framework_ids: ["f1"],
      }),
    );
    // What-if result (the synthetic pipeline) replaces the stored view; its
    // server-managed badge is a unique marker that it rendered.
    await waitFor(() => expect(screen.getByText("server-managed")).toBeTruthy());
  });

  it("re-runs what-if when a framework chip is toggled", async () => {
    render(
      <ProjectCompliancePreview
        slug="payments"
        frameworks={frameworks}
        assignedIDs={["f1"]}
        initial={stored}
      />,
    );
    fireEvent.click(screen.getByRole("switch"));
    await waitFor(() => expect(previewAction).toHaveBeenCalled());

    // Add SOC2 → recompute with both frameworks.
    fireEvent.click(screen.getByRole("button", { name: /Framework SOC2/i }));
    await waitFor(() =>
      expect(previewAction).toHaveBeenLastCalledWith({
        slug: "payments",
        framework_ids: ["f1", "f2"],
      }),
    );
  });
});
