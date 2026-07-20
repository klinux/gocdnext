import { notFound } from "next/navigation";
import type { Metadata } from "next";
import { Rocket } from "lucide-react";

import { EnvironmentCard } from "@/components/environments/environment-card.client";
import { DeployWatchesProvider } from "@/components/environments/deploy-watches-provider.client";
import { env } from "@/lib/env";
import {
  GocdnextAPIError,
  listDeployTargets,
  listEnvironments,
} from "@/server/queries/projects";
import type { DeployTarget } from "@/types/api";

type Params = { slug: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `Environments — ${slug}` };
}

// Environments + their current deploy change whenever a deploy job
// finishes; force-dynamic so the operator always sees the live "what's
// where now" instead of a cached snapshot.
export const dynamic = "force-dynamic";

export default async function EnvironmentsPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;

  // The environments endpoint resolves the project and 404s itself, so we skip a
  // redundant getProjectDetail round-trip. Fetch the native deploy targets in
  // parallel; that call is maintainer-gated and 403-tolerant (viewers get an empty
  // list), so the page stays viewer-readable and only maintainers see the native row.
  let environments;
  let targets: DeployTarget[] = [];
  try {
    const [envList, targetList] = await Promise.all([
      listEnvironments(slug),
      listDeployTargets(slug),
    ]);
    environments = envList.environments;
    targets = targetList.deploy_targets;
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  // deploy_targets are 1:1 with an environment by name.
  const targetByEnv = new Map(targets.map((t) => [t.environment, t]));

  return (
    <section className="space-y-6">
      <header>
        <p className="text-sm text-muted-foreground">
          What&apos;s deployed where, right now. An environment appears the
          first time a job with a <code className="text-xs">deploy:</code>{" "}
          block ships to it; the current version is the latest successful
          deploy. gocdnext tracks the deploy — a registered native provider
          (ArgoCD) drives it, or your plugin (Helm, kubectl) performs it.
        </p>
      </header>

      {environments.length === 0 ? (
        <EmptyState />
      ) : (
        // Client provider polls this project's in-flight native deploys once and feeds
        // each card its live chip; the cards stay RSC-composed here.
        <DeployWatchesProvider
          slug={slug}
          apiBaseURL={env.GOCDNEXT_PUBLIC_API_URL}
        >
          <div className="grid gap-4 sm:grid-cols-2">
            {environments.map((e) => (
              <EnvironmentCard
                key={e.id}
                slug={slug}
                environment={e}
                deployTarget={targetByEnv.get(e.name)}
                apiBaseURL={env.GOCDNEXT_PUBLIC_API_URL}
              />
            ))}
          </div>
        </DeployWatchesProvider>
      )}
    </section>
  );
}

function EmptyState() {
  return (
    <div className="flex flex-col items-center justify-center rounded-lg border border-dashed border-border bg-card py-16 text-center">
      <Rocket className="size-8 text-muted-foreground" aria-hidden />
      <h3 className="mt-4 text-sm font-medium">No environments yet</h3>
      <p className="mt-1 max-w-md text-sm text-muted-foreground">
        Add a <code className="text-xs">deploy:</code> block to a job (with an{" "}
        <code className="text-xs">environment:</code> name) and the first
        deploy will register it here.
      </p>
    </div>
  );
}
