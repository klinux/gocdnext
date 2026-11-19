import { notFound } from "next/navigation";
import type { Metadata, Route } from "next";

import { requireMaintainer } from "@/server/queries/access";
import { ProjectCronsEditor } from "@/components/projects/project-crons-editor.client";
import {
  GocdnextAPIError,
  getProjectDetail,
  listProjectCrons,
} from "@/server/queries/projects";

type Params = { slug: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `Schedules — ${slug}` };
}

export const dynamic = "force-dynamic";

export default async function ProjectCronsPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;
  // The schedules read is maintainer-gated; the tab is hidden from viewers and
  // this sends a direct hit back to the project instead of a 403 crash.
  await requireMaintainer(`/projects/${encodeURIComponent(slug)}` as Route);

  let detail;
  try {
    detail = await getProjectDetail(slug, 1);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  const { crons } = await listProjectCrons(slug);
  const pipelineOptions = detail.pipelines.map((p) => ({
    id: p.id,
    name: p.name,
  }));

  return (
    <section className="space-y-6">
      <ProjectCronsEditor
        slug={slug}
        initial={crons}
        pipelines={pipelineOptions}
      />
    </section>
  );
}
