import type { Metadata } from "next";

import { ProfilesManager } from "@/components/profiles/profiles-manager.client";
import { listAdminRunnerProfiles } from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Runner profiles — gocdnext",
};

// Profile mutations from this tab revalidate via the action; the
// extra force-dynamic keeps multi-tab edits in sync without a cache
// dance. Payload is small (tens of profiles, max).
export const dynamic = "force-dynamic";

export default async function RunnerProfilesPage() {
  const { profiles } = await listAdminRunnerProfiles();

  return (
    <section className="space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Runner profiles</h2>
        <p className="text-sm text-muted-foreground">
          Named bundles of execution policy — fallback image, default + max
          compute, and required agent tags. Pipelines reference them by name
          via{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            agent.profile
          </code>
          .
        </p>
      </div>

      <ProfilesManager initial={profiles} />
    </section>
  );
}
