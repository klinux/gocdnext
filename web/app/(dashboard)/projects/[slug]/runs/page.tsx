import { notFound } from "next/navigation";
import type { Metadata } from "next";

import { Pagination } from "@/components/shared/pagination";
import { RunsTable } from "@/components/runs/runs-table";
import {
  GocdnextAPIError,
  getProjectDetail,
  listGlobalRuns,
} from "@/server/queries/projects";

type Params = { slug: string };
type SearchParams = { offset?: string };

const PAGE_SIZE = 50;

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `${slug} — recent runs` };
}

export const dynamic = "force-dynamic";

// Recent runs tab. Uses the global /api/v1/runs endpoint with
// the `project=<slug>` filter so pagination + row shape stay
// identical to the /runs page. Dropping the previous
// getProjectDetail path here because that endpoint returns a
// bounded slice (no total / offset) — paginating it would need
// a separate query anyway, and reusing the global one keeps the
// two lists visually and mechanically the same.
export default async function ProjectRunsPage({
  params,
  searchParams,
}: {
  params: Promise<Params>;
  searchParams: Promise<SearchParams>;
}) {
  const { slug } = await params;
  const sp = await searchParams;
  const offset = sp.offset ? Math.max(0, Number.parseInt(sp.offset, 10)) : 0;

  // 404 early if the project is missing, so the runs fetch isn't
  // blamed for an invalid slug.
  try {
    await getProjectDetail(slug, 1);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  const data = await listGlobalRuns({
    limit: PAGE_SIZE,
    offset,
    project: slug,
  });

  return (
    <section className="space-y-4">
      <RunsTable
        runs={data.runs}
        variant="project"
        emptyMessage="No runs yet. Trigger one by pushing to a git material or calling the webhook directly."
      />

      <Pagination
        offset={offset}
        total={data.total}
        pageSize={PAGE_SIZE}
        basePath={`/projects/${slug}/runs`}
      />
    </section>
  );
}
