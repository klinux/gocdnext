import Link from "next/link";
import { notFound } from "next/navigation";
import type { Metadata } from "next";
import { ArrowLeft } from "lucide-react";

import { EnvironmentHistory } from "@/components/environments/environment-history.client";
import { Button } from "@/components/ui/button";
import { env } from "@/lib/env";
import { type AuthState, resolveAuthState } from "@/server/queries/auth";
import {
  GocdnextAPIError,
  listEnvironmentDeployments,
} from "@/server/queries/projects";

type Params = { slug: string; envId: string };

const PAGE_SIZE = 50;

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `${slug} — environment history` };
}

export const dynamic = "force-dynamic";

export default async function EnvironmentHistoryPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug, envId } = await params;

  let history;
  let auth: AuthState = { mode: "unknown" };
  try {
    const [historyPage, authState] = await Promise.all([
      listEnvironmentDeployments(slug, envId, { limit: PAGE_SIZE }),
      resolveAuthState().catch((): AuthState => ({ mode: "unknown" })),
    ]);
    history = historyPage;
    auth = authState;
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  if (!history.environment) notFound();

  const canManage =
    auth.mode === "disabled" ||
    (auth.mode === "authenticated" &&
      (auth.user.role === "admin" || auth.user.role === "maintainer"));

  return (
    <section className="space-y-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-2">
          <Button
            variant="ghost"
            size="sm"
            className="-ml-2"
            render={
              <Link href={`/projects/${slug}/environments`}>
                <ArrowLeft className="size-3.5" aria-hidden />
                Environments
              </Link>
            }
          />
          <div>
            <h1 className="text-xl font-semibold tracking-tight">
              {history.environment.name} deployment history
            </h1>
            <p className="mt-1 text-sm text-muted-foreground">
              Showing every recorded deploy attempt for this environment,
              newest first.
            </p>
          </div>
        </div>
        <span className="rounded-4xl border border-border bg-muted px-2 py-1 text-xs font-medium text-muted-foreground">
          {history.total ?? history.environment.total_deploys} deployment
          {(history.total ?? history.environment.total_deploys) === 1 ? "" : "s"}
        </span>
      </header>

      <EnvironmentHistory
        slug={slug}
        environmentId={envId}
        environmentName={history.environment.name}
        currentRevisionId={history.environment.current_revision_id}
        initial={history}
        apiBaseURL={env.GOCDNEXT_PUBLIC_API_URL}
        canManage={canManage}
      />
    </section>
  );
}
