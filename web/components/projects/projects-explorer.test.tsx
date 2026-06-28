import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ProjectsExplorer } from "./projects-explorer.client";
import type { ProjectSummary } from "@/types/api";

// jsdom's localStorage is misconfigured under this vitest setup; provide a
// minimal in-memory shim (the explorer reads/writes its view preference).
beforeEach(() => {
  const store: Record<string, string> = {};
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => store[k] ?? null,
    setItem: (k: string, v: string) => {
      store[k] = v;
    },
    removeItem: (k: string) => {
      delete store[k];
    },
    clear: () => {},
  });
});

// Isolate the toolbar/filter logic: stub the card/row/menu and router.
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh: vi.fn() }) }));
vi.mock("@/components/projects/project-card", () => ({
  ProjectCard: ({ project }: { project: ProjectSummary }) => <div>{project.name}</div>,
}));
vi.mock("@/components/projects/project-row", () => ({
  ProjectRow: ({ project }: { project: ProjectSummary }) => <div>{project.name}</div>,
}));
vi.mock("@/components/projects/visible-projects-menu.client", () => ({
  VisibleProjectsMenu: () => null,
}));

function proj(over: Partial<ProjectSummary> & Pick<ProjectSummary, "id" | "slug" | "name">): ProjectSummary {
  return {
    created_at: "", updated_at: "", pipeline_count: 1, run_count: 1,
    status: "success", ...over,
  };
}

const projects: ProjectSummary[] = [
  proj({ id: "1", slug: "pay", name: "Payments", labels: [{ key: "team", value: "payments" }] }),
  proj({ id: "2", slug: "web", name: "Web", labels: [{ key: "team", value: "web" }] }),
];

describe("ProjectsExplorer label filter", () => {
  it("renders a label chip per key:value and filters by it", () => {
    render(<ProjectsExplorer projects={projects} initialHiddenProjects={[]} />);

    // Both projects shown initially.
    expect(screen.getByText("Payments")).toBeTruthy();
    expect(screen.getByText("Web")).toBeTruthy();

    // A chip exists for team:payments — click it to filter.
    fireEvent.click(screen.getByRole("button", { name: /team:payments/i }));

    expect(screen.getByText("Payments")).toBeTruthy();
    expect(screen.queryByText("Web")).toBeNull();
  });
});
