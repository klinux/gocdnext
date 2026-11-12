import { notFound } from "next/navigation";
import type { Metadata } from "next";

import { ProjectPollSettings } from "@/components/projects/project-poll-settings.client";
import { ProjectLogArchiveSettings } from "@/components/projects/project-log-archive-settings.client";
import { ProjectGithubChecksSettings } from "@/components/projects/project-github-checks-settings.client";
import { ProjectLabelsCard } from "@/components/projects/project-labels.client";
import { ProjectComplianceCard } from "@/components/projects/project-compliance.client";
import { ProjectCompliancePreview } from "@/components/projects/project-compliance-preview.client";
import { ProjectRequiredChecks } from "@/components/projects/project-required-checks.client";
import { ProjectStatusBadgeSettings } from "@/components/projects/project-status-badge-settings.client";
import { ProjectBuildCacheSettings } from "@/components/projects/project-build-cache-settings.client";
import {
  ProjectSettingsTabs,
  type ProjectSettingsTab,
} from "@/components/projects/project-settings-tabs.client";
import {
  GocdnextAPIError,
  getProjectBadge,
  getProjectBuildCache,
  getProjectDetail,
  getProjectLogArchive,
  getProjectCheckReporting,
  getProjectRequiredChecks,
  type CheckReportingMode,
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

  // GitHub check reporting mode (both|check_run|commit_status). Separate
  // endpoint; degrades to null (card hidden) if it can't be read.
  let checkReporting: Awaited<
    ReturnType<typeof getProjectCheckReporting>
  > | null = null;
  try {
    checkReporting = await getProjectCheckReporting(slug);
  } catch {
    checkReporting = null;
  }

  let badge: Awaited<ReturnType<typeof getProjectBadge>> | null = null;
  try {
    badge = await getProjectBadge(slug);
  } catch {
    badge = null;
  }

  let buildCache: Awaited<ReturnType<typeof getProjectBuildCache>> | null = null;
  try {
    buildCache = await getProjectBuildCache(slug);
  } catch {
    buildCache = null;
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

  // Required pipelines for PR merge is admin-only (the PUT writes the repo's
  // GitHub ruleset) and GitHub-only. Degrades to null (card hidden) on error or
  // a non-GitHub project.
  let requiredChecks: Awaited<
    ReturnType<typeof getProjectRequiredChecks>
  > | null = null;
  if (isAdmin) {
    try {
      const rc = await getProjectRequiredChecks(slug);
      requiredChecks = rc.provider === "github" ? rc : null;
    } catch {
      requiredChecks = null;
    }
  }

  // Group the sections into vertical tabs. Each tab only renders when it has at
  // least one available section, so the conditional logic that used to gate
  // individual cards now gates whole tabs (General always shows — labels +
  // polling are unconditional).
  const tabs: ProjectSettingsTab[] = [
    {
      value: "general",
      label: "General",
      icon: "general",
      content: (
        <>
          <ProjectLabelsCard
            slug={slug}
            initial={detail.project.labels ?? []}
          />
          <ProjectPollSettings
            slug={slug}
            initialIntervalNs={detail.scm_source?.poll_interval_ns ?? 0}
            hasScmSource={Boolean(detail.scm_source)}
          />
          {badge ? (
            <ProjectStatusBadgeSettings
              slug={slug}
              initialEnabled={badge.enabled}
            />
          ) : null}
        </>
      ),
    },
  ];

  if (checkReporting || requiredChecks) {
    tabs.push({
      value: "checks",
      label: "GitHub checks",
      icon: "checks",
      content: (
        <>
          {checkReporting ? (
            <ProjectGithubChecksSettings
              slug={slug}
              initialMode={checkReporting.mode}
              // default_mode is the server's single source of truth; the API
              // only ever returns a valid mode, so the cast is safe.
              defaultMode={checkReporting.default_mode as CheckReportingMode}
            />
          ) : null}
          {requiredChecks ? (
            <ProjectRequiredChecks slug={slug} initial={requiredChecks} />
          ) : null}
        </>
      ),
    });
  }

  if (archive || buildCache) {
    tabs.push({
      value: "build",
      label: "Build & cache",
      icon: "build",
      content: (
        <>
          {archive ? (
            <ProjectLogArchiveSettings
              slug={slug}
              initialEnabled={archive.enabled}
              globalPolicy={archive.global_policy ?? "auto"}
              hasArtifactBackend={archive.has_artifact_backend}
            />
          ) : null}
          {buildCache ? (
            <ProjectBuildCacheSettings
              slug={slug}
              initialEnabled={buildCache.enabled}
            />
          ) : null}
        </>
      ),
    });
  }

  if (isAdmin) {
    tabs.push({
      value: "compliance",
      label: "Compliance",
      icon: "compliance",
      content: (
        <>
          <ProjectComplianceCard
            slug={slug}
            frameworks={allFrameworks}
            assignedIDs={assignedIDs}
          />
          {/*
            Reset the preview's internal state when the assignment changes: saving
            frameworks above revalidates this page, so a new assignment streams
            fresh `assignedIDs` + `initial`. Keying on the assignment remounts the
            preview so it reflects the just-saved governance (and any in-progress
            what-if for the OLD set is correctly discarded). An unrelated
            revalidation keeps the same key, preserving an active what-if.
          */}
          <ProjectCompliancePreview
            key={[...assignedIDs].sort().join(",")}
            slug={slug}
            frameworks={allFrameworks}
            assignedIDs={assignedIDs}
            initial={preview}
          />
        </>
      ),
    });
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

      <ProjectSettingsTabs tabs={tabs} />
    </section>
  );
}
