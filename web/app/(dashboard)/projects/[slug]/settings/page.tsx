import { notFound } from "next/navigation";
import type { Metadata } from "next";

import { ProjectPollSettings } from "@/components/projects/project-poll-settings.client";
import { ProjectLogArchiveSettings } from "@/components/projects/project-log-archive-settings.client";
import { ProjectComplianceCard } from "@/components/projects/project-compliance.client";
import { ProjectCompliancePreview } from "@/components/projects/project-compliance-preview.client";
import {
  GocdnextAPIError,
  getProjectDetail,
  getProjectLogArchive,
} from "@/server/queries/projects";
import {
  getEffectivePipelinePreview,
  getProjectFrameworks,
  listComplianceFrameworks,
  type ComplianceFramework,
  type EffectivePipelinePreview,
} from "@/server/queries/admin";
import { resolveAuthState } from "@/server/queries/auth";

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

  // Compliance framework assignment is admin-only (the API routes are
  // RoleAdmin). Resolve the viewer's role and, when admin, load the framework
  // catalog + this project's current set. Failures degrade to hiding the card.
  const auth = await resolveAuthState();
  // auth "disabled" = single-user/dev mode where the server's admin middleware
  // lets every request through — so the assignment card must show too, matching
  // the backend (RequireMinRole is a no-op when auth is disabled).
  const isAdmin =
    auth.mode === "disabled" ||
    (auth.mode === "authenticated" && auth.user.role === "admin");
  let allFrameworks: ComplianceFramework[] = [];
  let assignedIDs: string[] = [];
  // Stored ("what runs today") preview is server-rendered; the what-if recompute
  // is interactive (client → action). Defaults to empty so a fetch failure just
  // hides the preview rather than breaking the page.
  let preview: EffectivePipelinePreview[] = [];
  if (isAdmin) {
    try {
      const [all, assigned, effective] = await Promise.all([
        listComplianceFrameworks(),
        getProjectFrameworks(slug),
        getEffectivePipelinePreview(slug),
      ]);
      allFrameworks = all;
      assignedIDs = assigned.map((f) => f.id);
      preview = effective;
    } catch {
      allFrameworks = [];
    }
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

      {isAdmin ? (
        <ProjectComplianceCard
          slug={slug}
          frameworks={allFrameworks}
          assignedIDs={assignedIDs}
        />
      ) : null}

      {isAdmin ? (
        <ProjectCompliancePreview
          slug={slug}
          frameworks={allFrameworks}
          assignedIDs={assignedIDs}
          initial={preview}
        />
      ) : null}
    </section>
  );
}
