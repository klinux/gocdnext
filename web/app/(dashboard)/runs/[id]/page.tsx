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

  // Pass the BROWSER-facing API URL to the client tree. Empty
  // string = same-origin: client fetches use relative paths and ride
  // the ingress that already routes /api/v1/... to the control plane.
  // env.GOCDNEXT_API_URL is the in-cluster URL used only by SSR
  // fetches above (getRunDetail) — never serialised into the page.
  return (
    <RunLive
      initial={initial}
      runId={id}
      apiBaseURL={env.GOCDNEXT_PUBLIC_API_URL}
    />
  );
}
