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
import { AuditDateRange } from "@/components/admin/audit-date-range.client";
import { EntityChip, type EntityKind } from "@/components/shared/entity-chip";
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
  from?: string;
  to?: string;
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
    from: params.from || undefined,
    to: params.to || undefined,
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
        from={params.from ?? ""}
        to={params.to ?? ""}
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
                    <AuditTargetCell
                      type={e.target_type}
                      id={e.target_id}
                      metadata={e.metadata}
                    />
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
          from: params.from,
          to: params.to,
        }}
      />
    </section>
  );
}

// FilterForm is a plain GET form so the active filters live in the
// URL — bookmarkable, shareable, survives a reload. The server
// re-renders the RSC with the new query params. A tiny preset
// row below the date inputs writes common windows ("Today",
// "7d", "30d") into the from/to fields before the user even
// clicks Filter — all via <label htmlFor=…> links that flip the
// value via uncontrolled DOM so the whole thing stays RSC.
function FilterForm({
  action,
  targetType,
  actor,
  from,
  to,
}: {
  action: string;
  targetType: string;
  actor: string;
  from: string;
  to: string;
}) {
  return (
    <form
      method="get"
      className="grid grid-cols-1 gap-2 rounded-lg border border-border bg-card p-3 sm:grid-cols-2 md:grid-cols-[repeat(4,minmax(0,1fr))_auto] md:items-end"
    >
      <div className="space-y-1">
        <Label htmlFor="audit-action" className="text-[11px] text-muted-foreground">
          Action
        </Label>
        <Input
          id="audit-action"
          type="text"
          name="action"
          defaultValue={action}
          placeholder="project.apply"
          className="h-8 font-mono text-xs"
        />
      </div>
      <div className="space-y-1">
        <Label
          htmlFor="audit-target-type"
          className="text-[11px] text-muted-foreground"
        >
          Target
        </Label>
        <Input
          id="audit-target-type"
          type="text"
          name="target_type"
          defaultValue={targetType}
          placeholder="project"
          className="h-8 font-mono text-xs"
        />
      </div>
      <div className="space-y-1">
        <Label
          htmlFor="audit-actor"
          className="text-[11px] text-muted-foreground"
        >
          Actor
        </Label>
        <Input
          id="audit-actor"
          type="text"
          name="actor"
          defaultValue={actor}
          placeholder="email…"
          className="h-8 text-xs"
        />
      </div>
      <AuditDateRange from={from} to={to} />
      <Button type="submit" size="sm" className="h-8">
        Filter
      </Button>
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

// AuditTargetCell renders the (target_type, target_id) pair as a
// typed EntityChip when the type is one we know how to label, and
// falls back to a plain badge+id pair for unknown types. Some
// audit actions stamp human-friendly metadata (project_slug,
// pipeline_name) that we prefer over raw UUIDs — the cell pulls
// those out when present so an admin scanning the log sees
// "deploy" instead of "5f1b…c8".
function AuditTargetCell({
  type,
  id,
  metadata,
}: {
  type?: string;
  id?: string;
  metadata?: Record<string, unknown>;
}) {
  if (!type) {
    return <span className="text-xs text-muted-foreground">—</span>;
  }
  const kind = mapTargetKind(type);
  const label = readableLabel(type, id, metadata);
  if (kind) {
    return <EntityChip kind={kind} label={label} title={id ?? label} />;
  }
  // Unknown target type — render as a neutral pair so the audit
  // log still reads, but signal that we haven't typed it yet.
  return (
    <div className="flex flex-col gap-0.5">
      <Badge variant="outline" className="w-fit text-[10px] uppercase tracking-wide">
        {type}
      </Badge>
      {id ? (
        <span className="truncate font-mono text-xs">{id}</span>
      ) : null}
    </div>
  );
}

function mapTargetKind(type: string): EntityKind | null {
  switch (type) {
    case "project":
      return "project";
    case "run":
    case "job_run":
      return "run";
    case "user":
      return "user";
    case "group":
      return "group";
    case "service_account":
      return "service_account";
    case "api_token":
      return "secret";
    case "secret":
    case "global_secret":
      return "secret";
    case "runner_profile":
      return "profile";
    case "scm_credential":
    case "auth_provider":
    case "vcs_integration":
      return "service_account";
    case "webhook_secret":
      return "secret";
    case "pipeline":
      return "pipeline";
    case "approval":
      return "run";
    default:
      return null;
  }
}

function readableLabel(
  type: string,
  id?: string,
  metadata?: Record<string, unknown>,
): string {
  if (!metadata) return id ?? type;
  // Prefer descriptive metadata fields the emitter populated. The
  // ordering is "most specific first": a project_slug beats a
  // generic name field; a counter beats an id when both are present.
  const candidates: string[] = [];
  if (typeof metadata["pipeline_name"] === "string") {
    candidates.push(metadata["pipeline_name"] as string);
  }
  if (typeof metadata["project_slug"] === "string") {
    candidates.push(metadata["project_slug"] as string);
  }
  if (typeof metadata["name"] === "string") {
    candidates.push(metadata["name"] as string);
  }
  if (typeof metadata["email"] === "string") {
    candidates.push(metadata["email"] as string);
  }
  if (candidates.length > 0) return candidates[0]!;
  // Counters are numeric — show "#N" so it's clear it's a sequence.
  if (typeof metadata["counter"] === "number") {
    return `#${metadata["counter"]}`;
  }
  // Fall through to id slice — first 8 chars of a UUID is enough
  // to scan and click-to-copy, doesn't overflow narrow columns.
  if (id && id.length > 12) return id.slice(0, 8);
  return id ?? type;
}
