import type { Metadata, Route } from "next";
import Link from "next/link";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { StatusPill } from "@/components/shared/status-pill";
import { listWebhookDeliveries } from "@/server/queries/admin";
import { cn } from "@/lib/utils";
import type { StatusTone } from "@/lib/status";
import type { WebhookDeliverySummary } from "@/types/api";

export const metadata: Metadata = {
  title: "Settings — Webhooks",
};

const PAGE_SIZE = 50;

// Three providers gocdnext shipped multi-provider support for —
// the old list was github-only, leaving gitlab + bitbucket
// invisible after they landed. Keep "All" first so an unfiltered
// view is the default visual state.
const PROVIDERS = [
  { label: "All", value: "" },
  { label: "GitHub", value: "github" },
  { label: "GitLab", value: "gitlab" },
  { label: "Bitbucket", value: "bitbucket" },
];

const STATUSES = [
  { label: "All", value: "" },
  { label: "Accepted", value: "accepted", tone: "success" as StatusTone },
  { label: "Ignored", value: "ignored", tone: "neutral" as StatusTone },
  { label: "Rejected", value: "rejected", tone: "failed" as StatusTone },
  { label: "Error", value: "error", tone: "warning" as StatusTone },
];

type Props = {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
};

export default async function WebhooksPage({ searchParams }: Props) {
  const sp = await searchParams;
  const provider = strParam(sp.provider);
  const status = strParam(sp.status);
  const offset = numParam(sp.offset, 0);

  const res = await listWebhookDeliveries({
    provider,
    status,
    limit: PAGE_SIZE,
    offset,
  });

  const prevOffset = Math.max(0, offset - PAGE_SIZE);
  const hasMore = offset + res.deliveries.length < res.total;
  const anyActive = Boolean(provider || status);

  return (
    <div className="space-y-4">
      {/* Filter row: tight, two chip groups + clear */}
      <div className="space-y-2.5 rounded-lg border bg-card p-3">
        <FilterRow
          label="Provider"
          value={provider}
          options={PROVIDERS}
          buildHref={(v) => pageHref({ provider: v, status, offset: 0 })}
        />
        <FilterRow
          label="Status"
          value={status}
          options={STATUSES}
          buildHref={(v) => pageHref({ provider, status: v, offset: 0 })}
          tonedActive
        />
        {anyActive ? (
          <div className="flex items-center justify-between border-t pt-2.5">
            <span className="text-xs text-muted-foreground">
              {res.total.toLocaleString()} delivery
              {res.total === 1 ? "" : "ies"} matching
            </span>
            <Button
              variant="ghost"
              size="sm"
              className="h-7 text-xs"
              nativeButton={false}
              render={
                <Link href={"/settings/webhooks" as Route}>Clear filters</Link>
              }
            />
          </div>
        ) : (
          <div className="border-t pt-2.5 text-xs text-muted-foreground">
            {res.total.toLocaleString()} delivery
            {res.total === 1 ? "" : "ies"} total
          </div>
        )}
      </div>

      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm">
            {res.deliveries.length === 0
              ? "No deliveries"
              : `Showing ${offset + 1}–${offset + res.deliveries.length}`}
          </CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {res.deliveries.length === 0 ? (
            <div className="px-6 py-12 text-center text-sm text-muted-foreground">
              {anyActive
                ? "No deliveries match this filter."
                : "Webhook deliveries will appear here once a connected repo pings."}
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[160px]">Received</TableHead>
                  <TableHead className="w-24">Provider</TableHead>
                  <TableHead>Event</TableHead>
                  <TableHead className="w-28">Status</TableHead>
                  <TableHead className="w-20">HTTP</TableHead>
                  <TableHead>Error</TableHead>
                  <TableHead className="w-12 text-right">#</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {res.deliveries.map((d) => (
                  <Row key={d.id} d={d} />
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {res.total > PAGE_SIZE ? (
        <div className="flex items-center justify-between">
          <span className="text-xs text-muted-foreground tabular-nums">
            {offset + 1}–{offset + res.deliveries.length} of {res.total}
          </span>
          <div className="flex gap-2">
            <Button
              variant="outline"
              size="sm"
              nativeButton={false}
              disabled={offset === 0}
              render={
                <Link
                  href={
                    pageHref({
                      provider,
                      status,
                      offset: prevOffset,
                    }) as Route
                  }
                >
                  Previous
                </Link>
              }
            />
            <Button
              variant="outline"
              size="sm"
              nativeButton={false}
              disabled={!hasMore}
              render={
                <Link
                  href={
                    pageHref({
                      provider,
                      status,
                      offset: offset + PAGE_SIZE,
                    }) as Route
                  }
                >
                  Next
                </Link>
              }
            />
          </div>
        </div>
      ) : null}
    </div>
  );
}

type FilterOption = { label: string; value: string; tone?: StatusTone };

function FilterRow({
  label,
  value,
  options,
  buildHref,
  tonedActive,
}: {
  label: string;
  value: string;
  options: readonly FilterOption[];
  buildHref: (value: string) => string;
  tonedActive?: boolean;
}) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <span className="mr-1 w-16 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      {options.map((opt) => {
        const active = value === opt.value;
        return (
          <Link key={opt.label} href={buildHref(opt.value) as Route}>
            <span
              className={cn(
                "inline-flex h-6 cursor-pointer items-center rounded-md border px-2.5 text-xs transition-colors",
                active
                  ? tonedActive && opt.tone
                    ? toneActiveClass(opt.tone)
                    : "border-foreground bg-foreground text-background"
                  : "border-border bg-transparent text-foreground hover:bg-muted",
              )}
            >
              {opt.label}
            </span>
          </Link>
        );
      })}
    </div>
  );
}

function toneActiveClass(tone: StatusTone): string {
  switch (tone) {
    case "success":
      return "border-emerald-500/40 bg-emerald-500/15 text-emerald-700 dark:text-emerald-300";
    case "failed":
      return "border-rose-500/40 bg-rose-500/15 text-rose-700 dark:text-rose-300";
    case "warning":
      return "border-amber-500/40 bg-amber-500/15 text-amber-700 dark:text-amber-300";
    default:
      return "border-foreground bg-foreground text-background";
  }
}

function Row({ d }: { d: WebhookDeliverySummary }) {
  return (
    <TableRow className="hover:bg-muted/40">
      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
        {fmtAt(d.received_at)}
      </TableCell>
      <TableCell className="font-mono text-xs capitalize">
        {d.provider}
      </TableCell>
      <TableCell className="font-mono text-xs">{d.event}</TableCell>
      <TableCell>
        <StatusPill tone={statusTone(d.status)}>{d.status}</StatusPill>
      </TableCell>
      <TableCell className="font-mono text-xs">{d.http_status}</TableCell>
      <TableCell
        className="max-w-[280px] truncate text-xs text-muted-foreground"
        title={d.error}
      >
        {d.error ?? "—"}
      </TableCell>
      <TableCell className="text-right font-mono text-xs">{d.id}</TableCell>
    </TableRow>
  );
}

function statusTone(status: WebhookDeliverySummary["status"]): StatusTone {
  switch (status) {
    case "accepted":
      return "success";
    case "rejected":
      return "failed";
    case "error":
      return "warning";
    default:
      return "neutral";
  }
}

function pageHref({
  provider,
  status,
  offset,
}: {
  provider: string;
  status: string;
  offset: number;
}) {
  const qs = new URLSearchParams();
  if (provider) qs.set("provider", provider);
  if (status) qs.set("status", status);
  if (offset > 0) qs.set("offset", String(offset));
  const q = qs.toString();
  return q ? `/settings/webhooks?${q}` : "/settings/webhooks";
}

function strParam(v: string | string[] | undefined) {
  if (Array.isArray(v)) return v[0] ?? "";
  return v ?? "";
}

function numParam(v: string | string[] | undefined, fallback: number) {
  const raw = Array.isArray(v) ? v[0] : v;
  const n = Number(raw);
  return Number.isFinite(n) && n >= 0 ? n : fallback;
}

function fmtAt(at: string) {
  try {
    return new Date(at).toLocaleString();
  } catch {
    return at;
  }
}
