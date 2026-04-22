import { notFound } from "next/navigation";
import type { Metadata } from "next";

import { PipelineFlowExplorer } from "@/components/pipelines/pipeline-flow-explorer.client";
import {
  GocdnextAPIError,
  getProjectDetail,
} from "@/server/queries/projects";

type Params = { slug: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `${slug} — gocdnext` };
}

export const dynamic = "force-dynamic";

// Pipelines tab body. Layout owns the header + tabs strip, so
// this page renders only the DAG + a per-pipeline count hint.
export default async function ProjectPipelinesPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;

  let detail;
  try {
    detail = await getProjectDetail(slug);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  return (
    <PipelineFlowExplorer
      projectSlug={detail.project.slug}
      pipelines={detail.pipelines}
      edges={detail.edges ?? []}
      runs={detail.runs}
    />
  );
}
