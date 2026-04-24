import type { Metadata } from "next";
import { ClipboardList } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Pagination } from "@/components/shared/pagination";
import { RelativeTime } from "@/components/shared/relative-time";
import { listAuditEvents } from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Audit log — gocdnext",
};

export const dynamic = "force-dynamic";

const PAGE_SIZE = 25;

type SearchParams = Promise<{
  action?: string;
  target_type?: string;
  actor?: string;
  offset?: string;
}>;

export default async function AuditPage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const params = await searchParams;
  const offset = params.offset
    ? Math.max(0, Number.parseInt(params.offset, 10))
    : 0;

  const { events, total } = await listAuditEvents({
    action: params.action || undefined,
    targetType: params.target_type || undefined,
    actor: params.actor || undefined,
    limit: PAGE_SIZE,
    offset,
  });

  return (
    <section className="space-y-6">
      <header className="space-y-1">
        <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <ClipboardList className="h-6 w-6" aria-hidden />
          Audit log
        </h1>
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
      />

      {events.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border py-12 text-center text-sm text-muted-foreground">
          No events match. Try widening the filters.
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-border bg-card">
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
                <TableRow key={e.id} className="font-mono text-xs">
                  <TableCell className="whitespace-nowrap text-muted-foreground">
                    <RelativeTime at={e.at} />
                  </TableCell>
                  <TableCell>
                    {e.actor_email ? (
                      <span className="font-mono text-xs">{e.actor_email}</span>
                    ) : (
                      <Badge variant="outline" className="font-normal">
                        system
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell>
                    <Badge
                      variant="secondary"
                      className="font-mono text-[11px]"
                    >
                      {e.action}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-col">
                      <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
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
        </div>
      )}

      <Pagination
        offset={offset}
        total={total}
        pageSize={PAGE_SIZE}
        basePath="/admin/audit"
        params={{
          action: params.action,
          target_type: params.target_type,
          actor: params.actor,
        }}
      />
    </section>
  );
}

// FilterForm is a plain GET form so the active filters live in the
// URL — bookmarkable, shareable, survives a reload. The server
// re-renders the RSC with the new query params. Wrapped in the
// same border + bg-card shell as the other shadcn-flavoured list
// pages so the chrome reads consistently across /runs,
// /projects/[slug]/runs, and /admin/audit.
function FilterForm({
  action,
  targetType,
  actor,
}: {
  action: string;
  targetType: string;
  actor: string;
}) {
  return (
    <form
      method="get"
      className="grid gap-3 rounded-lg border border-border bg-card p-4 sm:grid-cols-[repeat(3,1fr)_auto]"
    >
      <div className="space-y-1.5">
        <Label htmlFor="audit-action" className="text-xs">
          Action
        </Label>
        <Input
          id="audit-action"
          type="text"
          name="action"
          defaultValue={action}
          placeholder="e.g. project.apply"
          className="font-mono text-xs"
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="audit-target-type" className="text-xs">
          Target type
        </Label>
        <Input
          id="audit-target-type"
          type="text"
          name="target_type"
          defaultValue={targetType}
          placeholder="e.g. project"
          className="font-mono text-xs"
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="audit-actor" className="text-xs">
          Actor (email)
        </Label>
        <Input
          id="audit-actor"
          type="text"
          name="actor"
          defaultValue={actor}
          placeholder="partial match"
          className="text-xs"
        />
      </div>
      <div className="flex items-end">
        <Button type="submit" size="sm">
          Filter
        </Button>
      </div>
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
