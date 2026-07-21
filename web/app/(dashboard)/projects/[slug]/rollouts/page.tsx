import type { Metadata } from "next";

import { RolloutSelector } from "@/components/rollouts/rollout-selector.client";
import { RolloutsHeader } from "@/components/rollouts/rollouts-header";
import { RolloutsLive } from "@/components/rollouts/rollouts-live.client";
import { env } from "@/lib/env";
import { GocdnextAPIError, listRollouts } from "@/server/queries/projects";
import type { RolloutsList } from "@/types/api";

type Params = { slug: string };
type Search = Record<string, string | string[] | undefined>;

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `Rollouts — ${slug}` };
}

// Rollouts reflect live cluster state, so never cache the shell.
export const dynamic = "force-dynamic";

// firstParam collapses a possibly-repeated query param to a single trimmed
// value (Next hands ?cluster=a&cluster=b through as an array).
function firstParam(v: string | string[] | undefined): string {
  if (Array.isArray(v)) return v[0]?.trim() ?? "";
  return v?.trim() ?? "";
}

export default async function RolloutsPage({
  params,
  searchParams,
}: {
  params: Promise<Params>;
  searchParams: Promise<Search>;
}) {
  const { slug } = await params;
  const sp = await searchParams;
  const cluster = firstParam(sp.cluster);
  const namespace = firstParam(sp.namespace);
  const basePath = `/projects/${slug}/rollouts`;

  // Needs-params state: ask for cluster + namespace instead of hard-failing.
  if (!cluster || !namespace) {
    return (
      <section className="space-y-6">
        <RolloutsHeader />
        <RolloutSelector
          basePath={basePath}
          defaultCluster={cluster}
          defaultNamespace={namespace}
        />
      </section>
    );
  }

  let initialData: RolloutsList;
  try {
    initialData = await listRollouts(slug, cluster, namespace);
  } catch (err) {
    // The layout already resolved the project, so a 404 here is the rollouts
    // endpoint's collapsed "cluster not found or not accessible" (it shares the
    // 404 status with a missing project) — recoverable, so keep the selector
    // instead of a dead-end notFound(). 401/403 = access, 501 = provider not
    // wired. Anything else is a real fault — propagate it.
    if (
      err instanceof GocdnextAPIError &&
      (err.status === 401 ||
        err.status === 403 ||
        err.status === 404 ||
        err.status === 501)
    ) {
      return (
        <section className="space-y-6">
          <RolloutsHeader />
          <AccessNote status={err.status} />
          <RolloutSelector
            basePath={basePath}
            defaultCluster={cluster}
            defaultNamespace={namespace}
          />
        </section>
      );
    }
    throw err;
  }

  return (
    <RolloutsLive
      slug={slug}
      cluster={cluster}
      namespace={namespace}
      apiBaseURL={env.GOCDNEXT_PUBLIC_API_URL}
      initialData={initialData}
    />
  );
}

function AccessNote({ status }: { status: number }) {
  const msg =
    status === 501
      ? "No native rollout provider is wired on this server yet."
      : status === 404
        ? "Cluster not found or not accessible — check the cluster name and namespace."
        : "You need maintainer access to view rollouts for this project.";
  return (
    <div className="rounded-xl border border-amber-500/40 bg-amber-500/10 p-4 text-sm text-amber-700 dark:text-amber-300">
      {msg}
    </div>
  );
}
