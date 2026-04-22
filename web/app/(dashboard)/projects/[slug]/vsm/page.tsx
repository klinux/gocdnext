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

  if (vsm.nodes.length === 0) {
    return (
      <div className="rounded-md border border-dashed border-border p-10 text-center text-sm text-muted-foreground">
        This project has no pipelines yet. Apply one with{" "}
        <code className="rounded bg-muted px-1 py-0.5">gocdnext apply</code>.
      </div>
    );
  }
  return (
    <div className="overflow-hidden rounded-md border border-border bg-card">
      <VSMGraph vsm={vsm} />
    </div>
  );
}
