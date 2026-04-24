import type { Metadata } from "next";

import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { listAuditEvents } from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — Audit log",
};

export const dynamic = "force-dynamic";

type SearchParams = Promise<{
  action?: string;
  target_type?: string;
  actor?: string;
  limit?: string;
}>;

export default async function AuditPage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const params = await searchParams;
  const limit = parseLimit(params.limit);
  const { events } = await listAuditEvents({
    action: params.action || undefined,
    targetType: params.target_type || undefined,
    actor: params.actor || undefined,
    limit,
  });

  return (
    <section className="space-y-6">
      <header className="space-y-1">
        <p className="text-sm text-muted-foreground">
          Every RBAC&apos;d write emits an event here — project apply,
          secrets, cache purge, runs, approvals, role changes.
          System-driven rows (webhook-auto-created runs, cron
          triggers) carry an empty actor so operators can distinguish
          human vs automation activity at a glance.
        </p>
      </header>

      <FilterForm
        action={params.action ?? ""}
        targetType={params.target_type ?? ""}
        actor={params.actor ?? ""}
        limit={limit}
      />

      {events.length === 0 ? (
        <p className="rounded-md border border-dashed border-border p-6 text-center text-sm text-muted-foreground">
          No events match. Try widening the filters or bumping the
          limit.
        </p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-[160px]">When</TableHead>
              <TableHead>Actor</TableHead>
              <TableHead>Action</TableHead>
              <TableHead>Target</TableHead>
              <TableHead>Metadata</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {events.map((e) => (
              <TableRow key={e.id}>
                <TableCell className="whitespace-nowrap text-muted-foreground">
                  <RelativeTime at={e.at} />
                </TableCell>
                <TableCell>
                  {e.actor_email ? (
                    <span className="font-mono text-sm">{e.actor_email}</span>
                  ) : (
                    <Badge variant="outline" className="font-normal">
                      system
                    </Badge>
                  )}
                </TableCell>
                <TableCell>
                  <Badge variant="secondary" className="font-mono text-[11px]">
                    {e.action}
                  </Badge>
                </TableCell>
                <TableCell>
                  <div className="flex flex-col">
                    <span className="text-xs uppercase tracking-wide text-muted-foreground">
                      {e.target_type}
                    </span>
                    {e.target_id ? (
                      <span className="truncate font-mono text-xs">
                        {e.target_id}
                      </span>
                    ) : null}
                  </div>
                </TableCell>
                <TableCell>
                  <MetadataCell metadata={e.metadata} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}

// FilterForm is a plain GET form so the active filters live in the
// URL — bookmarkable, shareable, survives a reload. The server
// re-renders the RSC with the new query params. No client code
// needed for filtering.
function FilterForm({
  action,
  targetType,
  actor,
  limit,
}: {
  action: string;
  targetType: string;
  actor: string;
  limit: number;
}) {
  return (
    <form
      method="get"
      className="grid gap-2 rounded-md border border-border bg-muted/30 p-3 text-sm sm:grid-cols-[repeat(4,1fr)_auto]"
    >
      <label className="flex flex-col gap-1">
        <span className="text-xs uppercase tracking-wide text-muted-foreground">
          Action
        </span>
        <input
          type="text"
          name="action"
          defaultValue={action}
          placeholder="e.g. project.apply"
          className="h-8 rounded border border-input bg-background px-2 font-mono text-xs"
        />
      </label>
      <label className="flex flex-col gap-1">
        <span className="text-xs uppercase tracking-wide text-muted-foreground">
          Target type
        </span>
        <input
          type="text"
          name="target_type"
          defaultValue={targetType}
          placeholder="e.g. project"
          className="h-8 rounded border border-input bg-background px-2 font-mono text-xs"
        />
      </label>
      <label className="flex flex-col gap-1">
        <span className="text-xs uppercase tracking-wide text-muted-foreground">
          Actor (email)
        </span>
        <input
          type="text"
          name="actor"
          defaultValue={actor}
          placeholder="partial match"
          className="h-8 rounded border border-input bg-background px-2 text-xs"
        />
      </label>
      <label className="flex flex-col gap-1">
        <span className="text-xs uppercase tracking-wide text-muted-foreground">
          Limit
        </span>
        <input
          type="number"
          name="limit"
          defaultValue={limit}
          min={1}
          max={500}
          className="h-8 rounded border border-input bg-background px-2 text-xs"
        />
      </label>
      <button
        type="submit"
        className="h-8 self-end rounded-md border border-input bg-primary px-3 text-xs font-medium text-primary-foreground hover:opacity-90"
      >
        Filter
      </button>
    </form>
  );
}

// MetadataCell renders the JSONB blob as one key=value line per
// entry, truncated to keep table rows compact. A "+N more" tail
// hides long blobs so a run.trigger with a big revisions payload
// doesn't distort the row height.
function MetadataCell({ metadata }: { metadata: Record<string, unknown> }) {
  const entries = Object.entries(metadata ?? {});
  if (entries.length === 0) {
    return <span className="text-xs text-muted-foreground">—</span>;
  }
  const head = entries.slice(0, 3);
  const rest = entries.length - head.length;
  return (
    <div className="flex flex-col gap-0.5 text-xs">
      {head.map(([k, v]) => (
        <div key={k} className="flex gap-1">
          <span className="text-muted-foreground">{k}</span>
          <span className="truncate font-mono">{String(v)}</span>
        </div>
      ))}
      {rest > 0 ? (
        <span className="text-muted-foreground">+{rest} more</span>
      ) : null}
    </div>
  );
}

function parseLimit(raw?: string): number {
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0) return 100;
  return Math.min(Math.max(n, 1), 500);
}
