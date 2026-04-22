import Link from "next/link";
import { notFound } from "next/navigation";
import type { Metadata, Route } from "next";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { durationBetween, formatDurationSeconds } from "@/lib/format";
import {
  GocdnextAPIError,
  getProjectDetail,
} from "@/server/queries/projects";

type Params = { slug: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `${slug} — recent runs` };
}

export const dynamic = "force-dynamic";

// Recent runs tab. Pulls the detail feed with a larger run limit
// than the layout's lean fetch (which only needed the metadata),
// so this page actually lists activity instead of the last-one
// placeholder the layout used for the header chrome.
export default async function ProjectRunsPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;

  let detail;
  try {
    detail = await getProjectDetail(slug, 50);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  return (
    <div className="space-y-3">
      {detail.runs.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No runs yet. Trigger one by pushing to a git material or calling the
          webhook directly.
        </p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-16">#</TableHead>
              <TableHead>Pipeline</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Cause</TableHead>
              <TableHead>Started</TableHead>
              <TableHead>Duration</TableHead>
              <TableHead className="text-right">Triggered by</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {detail.runs.map((r) => (
              <TableRow key={r.id} className="font-mono text-xs">
                <TableCell className="font-semibold">#{r.counter}</TableCell>
                <TableCell>
                  <Link
                    href={`/runs/${r.id}` as Route}
                    className="hover:underline"
                  >
                    {r.pipeline_name}
                  </Link>
                </TableCell>
                <TableCell>
                  <StatusBadge status={r.status} />
                </TableCell>
                <TableCell className="text-muted-foreground">{r.cause}</TableCell>
                <TableCell>
                  <RelativeTime at={r.started_at ?? r.created_at} />
                </TableCell>
                <TableCell>
                  {formatDurationSeconds(
                    durationBetween(r.started_at, r.finished_at),
                  )}
                </TableCell>
                <TableCell className="text-right text-muted-foreground">
                  {r.triggered_by ?? "—"}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
