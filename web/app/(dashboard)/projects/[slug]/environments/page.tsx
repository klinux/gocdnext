import { notFound } from "next/navigation";
import type { Metadata } from "next";
import { Rocket } from "lucide-react";

import { EnvironmentCard } from "@/components/environments/environment-card.client";
import { env } from "@/lib/env";
import { GocdnextAPIError, listEnvironments } from "@/server/queries/projects";

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

  // Single request: the environments endpoint resolves the project and
  // 404s itself, so we skip a redundant getProjectDetail round-trip.
  let environments;
  try {
    ({ environments } = await listEnvironments(slug));
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  return (
    <section className="space-y-6">
      <header>
        <p className="text-sm text-muted-foreground">
          What&apos;s deployed where, right now. An environment appears the
          first time a job with a <code className="text-xs">deploy:</code>{" "}
          block ships to it; the current version is the latest successful
          deploy. gocdnext tracks the deploy — your plugin (Argo, Helm,
          kubectl) performs it.
        </p>
      </header>

      {environments.length === 0 ? (
        <EmptyState />
      ) : (
        <div className="grid gap-4 sm:grid-cols-2">
          {environments.map((e) => (
            <EnvironmentCard
              key={e.id}
              slug={slug}
              environment={e}
              apiBaseURL={env.GOCDNEXT_PUBLIC_API_URL}
            />
          ))}
        </div>
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
