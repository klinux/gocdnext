import Link from "next/link";
import { notFound } from "next/navigation";
import type { ReactNode } from "react";
import { ChevronRight } from "lucide-react";

import { ProjectActionsMenu } from "@/components/projects/project-actions-menu.client";
import { ProjectTabs } from "@/components/projects/project-tabs.client";
import {
  GocdnextAPIError,
  getProjectDetail,
} from "@/server/queries/projects";

type Params = { slug: string };

// Shared shell for /projects/[slug]/** — header (breadcrumb,
// name, actions) sits outside the tab body so navigating
// between Pipelines / VSM / Secrets / Runs doesn't flicker the
// chrome. Each sub-page fetches whatever extra data it needs;
// this layout just grabs the minimum for the header and the
// actions menu.
export default async function ProjectLayout({
  children,
  params,
}: {
  children: ReactNode;
  params: Promise<Params>;
}) {
  const { slug } = await params;

  let detail;
  try {
    // runs=1 keeps the layout's fetch lean — it only needs
    // project metadata + scm_source + counts for the actions
    // menu. Sub-pages that need the richer runs/pipelines data
    // re-fetch at their own depth.
    detail = await getProjectDetail(slug, 1);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  return (
    <section className="space-y-6">
      <header>
        <nav aria-label="Breadcrumb" className="text-xs text-muted-foreground">
          <Link href="/" className="hover:text-foreground">
            Projects
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <span>{detail.project.slug}</span>
        </nav>
        <div className="mt-1 flex items-start justify-between gap-4">
          <div className="min-w-0 flex-1">
            <h2 className="truncate text-2xl font-semibold tracking-tight">
              {detail.project.name}
            </h2>
            {detail.project.description ? (
              <p className="mt-1 text-sm text-muted-foreground">
                {detail.project.description}
              </p>
            ) : null}
          </div>
          <ProjectActionsMenu
            project={detail.project}
            scmSource={detail.scm_source}
            pipelineCount={detail.pipelines.length}
            runCount={detail.runs.length}
          />
        </div>
      </header>

      <ProjectTabs slug={slug} />

      <div>{children}</div>
    </section>
  );
}
