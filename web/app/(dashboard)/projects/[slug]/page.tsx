import Link from "next/link";
import { notFound } from "next/navigation";
import type { Metadata, Route } from "next";
import { ChevronRight } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Separator } from "@/components/ui/separator";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { TriggerPipelineButton } from "@/components/pipelines/trigger-pipeline-button.client";
import { formatDurationSeconds, durationBetween } from "@/lib/format";
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
  return { title: `${slug} — gocdnext` };
}

export const dynamic = "force-dynamic";

export default async function ProjectDetailPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;

  let detail;
  try {
    detail = await getProjectDetail(slug);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  return (
    <section className="space-y-8">
      <header>
        <nav aria-label="Breadcrumb" className="text-xs text-muted-foreground">
          <Link href="/" className="hover:text-foreground">
            Projects
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <span>{detail.project.slug}</span>
        </nav>
        <div className="mt-1 flex items-baseline justify-between gap-4">
          <h2 className="text-2xl font-semibold tracking-tight">
            {detail.project.name}
          </h2>
          <div className="flex items-center gap-4 text-sm">
            <Link
              href={`/projects/${detail.project.slug}/vsm` as Route}
              className="text-muted-foreground hover:text-foreground underline-offset-4 hover:underline"
            >
              View VSM →
            </Link>
            <Link
              href={`/projects/${detail.project.slug}/secrets` as Route}
              className="text-muted-foreground hover:text-foreground underline-offset-4 hover:underline"
            >
              Manage secrets →
            </Link>
          </div>
        </div>
        {detail.project.description ? (
          <p className="mt-1 text-sm text-muted-foreground">
            {detail.project.description}
          </p>
        ) : null}
      </header>

      <Card>
        <CardHeader>
          <CardTitle>Pipelines</CardTitle>
          <CardDescription>
            {detail.pipelines.length} definition
            {detail.pipelines.length === 1 ? "" : "s"}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {detail.pipelines.length === 0 ? (
            <p className="text-sm text-muted-foreground">No pipelines.</p>
          ) : (
            <ul className="divide-y divide-border">
              {detail.pipelines.map((pl) => (
                <li
                  key={pl.id}
                  className="flex items-center justify-between py-2 text-sm"
                >
                  <div className="flex flex-col">
                    <span className="font-mono">{pl.name}</span>
                    <span className="text-xs text-muted-foreground">
                      v{pl.definition_version} · updated{" "}
                      <RelativeTime at={pl.updated_at} />
                    </span>
                  </div>
                  <TriggerPipelineButton
                    pipelineId={pl.id}
                    pipelineName={pl.name}
                    projectSlug={detail.project.slug}
                  />
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      <Separator />

      <section>
        <header className="mb-3 flex items-baseline justify-between">
          <h3 className="text-lg font-semibold tracking-tight">Recent runs</h3>
          <span className="text-xs text-muted-foreground">
            {detail.runs.length} shown
          </span>
        </header>
        {detail.runs.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No runs yet. Trigger one by pushing to a git material or calling the
            webhook directly.
          </p>
        ) : (
          <RunsTable runs={detail.runs} />
        )}
      </section>
    </section>
  );
}

function RunsTable({
  runs,
}: {
  runs: Awaited<ReturnType<typeof getProjectDetail>>["runs"];
}) {
  return (
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
        {runs.map((r) => (
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
  );
}
