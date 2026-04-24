import { notFound } from "next/navigation";
import type { Metadata } from "next";

import { Toaster } from "@/components/ui/sonner";
import { ProjectNotificationsEditor } from "@/components/notifications/project-notifications-editor.client";
import {
  GocdnextAPIError,
  getProjectDetail,
  listProjectNotifications,
} from "@/server/queries/projects";

type Params = { slug: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `Notifications — ${slug}` };
}

// Force dynamic so a Save → revalidate cycle renders fresh state
// immediately. The payload is small (few entries, names + YAML
// snippets), cache staleness here is just friction.
export const dynamic = "force-dynamic";

export default async function ProjectNotificationsPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;

  try {
    await getProjectDetail(slug, 1);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  const { notifications } = await listProjectNotifications(slug);

  return (
    <section className="space-y-6">
      <Toaster position="top-right" richColors />
      <div className="space-y-1">
        <p className="text-sm text-muted-foreground">
          Post-run notifications inherited by every pipeline under this
          project — fired once the user stages finish, filtered by{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">on:</code>{" "}
          against the aggregated outcome. A pipeline that declares its own{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            notifications:
          </code>{" "}
          block overrides this list entirely (even an empty one — that&apos;s
          an explicit opt-out).
        </p>
      </div>
      <ProjectNotificationsEditor slug={slug} initial={notifications} />
    </section>
  );
}
