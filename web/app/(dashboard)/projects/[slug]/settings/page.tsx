import { notFound } from "next/navigation";
import type { Metadata } from "next";

import { ProjectPollSettings } from "@/components/projects/project-poll-settings.client";
import { ProjectLogArchiveSettings } from "@/components/projects/project-log-archive-settings.client";
import {
  GocdnextAPIError,
  getProjectDetail,
  getProjectLogArchive,
} from "@/server/queries/projects";

type Params = { slug: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `Settings — ${slug}` };
}

export const dynamic = "force-dynamic";

export default async function ProjectSettingsPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;

  let detail;
  try {
    detail = await getProjectDetail(slug, 1);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  // Archive settings come from a separate endpoint so the page works
  // for projects with neither artifact backend nor archive policy
  // configured (the response degrades gracefully).
  let archive: Awaited<ReturnType<typeof getProjectLogArchive>> | null = null;
  try {
    archive = await getProjectLogArchive(slug);
  } catch {
    archive = null;
  }

  return (
    <section className="space-y-6">
      <div className="space-y-1">
        <h3 className="text-lg font-semibold">Project settings</h3>
        <p className="text-sm text-muted-foreground">
          Project-level knobs that apply across every pipeline bound to this
          project.
        </p>
      </div>

      <ProjectPollSettings
        slug={slug}
        initialIntervalNs={detail.scm_source?.poll_interval_ns ?? 0}
        hasScmSource={Boolean(detail.scm_source)}
      />

      {archive ? (
        <ProjectLogArchiveSettings
          slug={slug}
          initialEnabled={archive.enabled}
          globalPolicy={archive.global_policy ?? "auto"}
          hasArtifactBackend={archive.has_artifact_backend}
        />
      ) : null}
    </section>
  );
}
