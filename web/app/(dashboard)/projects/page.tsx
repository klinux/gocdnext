import type { Metadata } from "next";

import { NewProjectDialog } from "@/components/projects/new-project-dialog.client";
import { ProjectsExplorer } from "@/components/projects/projects-explorer.client";
import { listProjects } from "@/server/queries/projects";
import { getUserPreferences } from "@/server/queries/account";

export const metadata: Metadata = {
  title: "Projects — gocdnext",
};

export const dynamic = "force-dynamic";

export default async function ProjectsPage() {
  // Fire-and-parallel: the projects list and the user's hide-list
  // are independent GETs and we need both before render. Missing
  // prefs (auth off, new user) resolves to an empty document — the
  // explorer handles that as "show all".
  const [projects, preferences] = await Promise.all([
    listProjects(),
    getUserPreferences(),
  ]);

  if (projects.length === 0) {
    return <EmptyState />;
  }

  return (
    <section className="space-y-6">
      <header className="flex items-baseline justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Projects</h2>
          <p className="text-sm text-muted-foreground">
            Browse, filter and open any project registered on this control
            plane. Click a card to view its pipelines and recent runs.
          </p>
        </div>
        <NewProjectDialog />
      </header>

      <ProjectsExplorer
        projects={projects}
        initialHiddenProjects={preferences.hidden_projects ?? []}
      />
    </section>
  );
}

function EmptyState() {
  return (
    <section className="mx-auto max-w-lg space-y-4 rounded-lg border border-dashed border-border p-10 text-center">
      <h2 className="text-xl font-semibold">No projects yet</h2>
      <p className="text-sm text-muted-foreground">
        Click the button to create one with a template, connect an existing
        repo, or go empty and add pipelines later. The CLI path (
        <code className="rounded bg-muted px-1 py-0.5">gocdnext apply</code>)
        also works for power users.
      </p>
      <div className="flex justify-center pt-2">
        <NewProjectDialog />
      </div>
    </section>
  );
}
