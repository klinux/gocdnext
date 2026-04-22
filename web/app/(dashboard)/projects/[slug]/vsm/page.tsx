import { notFound } from "next/navigation";
import type { Metadata } from "next";

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
    <div className="space-y-3">
      <div className="flex items-baseline justify-between">
        <h3 className="text-lg font-semibold tracking-tight">Value Stream</h3>
        <span className="text-xs text-muted-foreground">
          {vsm.nodes.length} pipeline{vsm.nodes.length === 1 ? "" : "s"}
          {vsm.edges.length > 0
            ? ` · ${vsm.edges.length} upstream edge${vsm.edges.length === 1 ? "" : "s"}`
            : ""}
        </span>
      </div>
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
    </div>
  );
}
