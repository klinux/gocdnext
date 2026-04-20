import Link from "next/link";
import { notFound } from "next/navigation";
import type { Metadata, Route } from "next";
import { ChevronRight } from "lucide-react";

import {
  GocdnextAPIError,
  getProjectVSM,
} from "@/server/queries/projects";
import { VSMGraph } from "@/components/vsm/vsm-graph.client";

type Params = { slug: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `${slug} VSM — gocdnext` };
}

export const dynamic = "force-dynamic";

export default async function VSMPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;
  let vsm;
  try {
    vsm = await getProjectVSM(slug);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  return (
    <section className="space-y-4">
      <header>
        <nav aria-label="Breadcrumb" className="text-xs text-muted-foreground">
          <Link href="/" className="hover:text-foreground">
            Projects
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <Link
            href={`/projects/${slug}` as Route}
            className="hover:text-foreground"
          >
            {slug}
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <span>VSM</span>
        </nav>
        <h2 className="mt-1 text-2xl font-semibold tracking-tight">
          {vsm.project_name}{" "}
          <span className="text-muted-foreground">Value Stream</span>
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          {vsm.nodes.length} pipeline{vsm.nodes.length === 1 ? "" : "s"}
          {vsm.edges.length > 0 ? (
            <>
              {" "}· {vsm.edges.length} upstream edge
              {vsm.edges.length === 1 ? "" : "s"}
            </>
          ) : null}
        </p>
      </header>

      {vsm.nodes.length === 0 ? (
        <div className="rounded-md border border-dashed border-border p-10 text-center text-sm text-muted-foreground">
          This project has no pipelines yet. Apply one with{" "}
          <code className="rounded bg-muted px-1 py-0.5">gocdnext apply</code>.
        </div>
      ) : (
        <div className="h-[70vh] rounded-md border border-border bg-card">
          <VSMGraph vsm={vsm} />
        </div>
      )}
    </section>
  );
}
