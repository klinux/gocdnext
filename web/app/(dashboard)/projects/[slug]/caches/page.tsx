import { notFound } from "next/navigation";
import type { Metadata } from "next";
import { HardDrive } from "lucide-react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { PurgeCacheButton } from "@/components/caches/purge-cache-button.client";
import { formatBytes } from "@/lib/format";
import {
  GocdnextAPIError,
  getProjectDetail,
  listCaches,
} from "@/server/queries/projects";

type Params = { slug: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `Caches — ${slug}` };
}

// Caches grow across runs and the sweeper only evicts on TTL
// or quota breach. Forcing dynamic ensures the operator always
// sees a fresh view after a manual purge, matching what the
// revalidatePath call does after purgeCache() succeeds.
export const dynamic = "force-dynamic";

export default async function CachesPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;

  try {
    await getProjectDetail(slug, 1);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  const data = await listCaches(slug);
  const caches = data.caches;

  return (
    <section className="space-y-6">
      <header className="flex items-start justify-between gap-4">
        <p className="text-sm text-muted-foreground">
          Pipeline caches persist tarred directories across runs, keyed by
          name. The sweeper evicts rows past the TTL or quota; manual purge
          forces an immediate refresh on the next run.
        </p>
        <div className="text-right text-sm">
          <div className="text-muted-foreground">Ready footprint</div>
          <div className="font-mono text-base">
            {formatBytes(data.total_bytes)}
          </div>
        </div>
      </header>

      {caches.length === 0 ? (
        <EmptyState />
      ) : (
        <div className="overflow-hidden rounded-lg border border-border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Key</TableHead>
                <TableHead>Size</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Last used</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {caches.map((c) => (
                <TableRow key={c.id}>
                  <TableCell className="font-mono">{c.key}</TableCell>
                  <TableCell className="font-mono text-muted-foreground">
                    {c.status === "ready" ? formatBytes(c.size_bytes) : "—"}
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={c.status} />
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    <RelativeTime at={c.last_accessed_at} />
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    <RelativeTime at={c.created_at} />
                  </TableCell>
                  <TableCell className="text-right">
                    <PurgeCacheButton
                      slug={slug}
                      cacheID={c.id}
                      cacheKey={c.key}
                    />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </section>
  );
}

function EmptyState() {
  return (
    <section className="mx-auto max-w-lg rounded-lg border border-dashed border-border p-10 text-center">
      <HardDrive className="mx-auto h-6 w-6 text-muted-foreground" aria-hidden />
      <h3 className="mt-3 text-lg font-semibold">No caches yet</h3>
      <p className="mt-2 text-sm text-muted-foreground">
        Declare <code className="rounded bg-muted px-1 py-0.5 text-xs">cache:</code>{" "}
        on a job in your pipeline YAML. The agent populates it on the first
        successful run and subsequent runs restore it before tasks execute.
      </p>
    </section>
  );
}

// StatusBadge mirrors the light pill style used in the runs
// table so an operator scanning multiple tabs recognises state
// at a glance. Pending is amber (in-flight / stuck), ready is
// plain (healthy default, no alert).
function StatusBadge({ status }: { status: string }) {
  const cls =
    status === "pending"
      ? "border-amber-400/60 bg-amber-400/10 text-amber-700 dark:text-amber-300"
      : "border-border bg-muted/40 text-muted-foreground";
  return (
    <span
      className={`inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium ${cls}`}
    >
      {status}
    </span>
  );
}
