import { notFound } from "next/navigation";
import type { Metadata } from "next";
import { env } from "@/lib/env";
import { RunLive } from "@/components/runs/run-live.client";
import {
  GocdnextAPIError,
  getRunDetail,
} from "@/server/queries/projects";

type Params = { id: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { id } = await params;
  return { title: `Run ${id.slice(0, 8)} — gocdnext` };
}

export const dynamic = "force-dynamic";

export default async function RunDetailPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { id } = await params;

  let initial;
  try {
    initial = await getRunDetail(id, 500);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  return (
    <RunLive
      initial={initial}
      runId={id}
      apiBaseURL={env.GOCDNEXT_API_URL}
    />
  );
}
