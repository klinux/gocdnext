import type { Metadata, Route } from "next";
import Link from "next/link";

import { Badge } from "@/components/ui/badge";
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
import { listWebhookDeliveries } from "@/server/queries/admin";
import { cn } from "@/lib/utils";
import type { WebhookDeliverySummary } from "@/types/api";

export const metadata: Metadata = {
  title: "Settings — Webhooks",
};

const PAGE_SIZE = 50;

const PROVIDERS: { label: string; value: string }[] = [
  { label: "All", value: "" },
  { label: "GitHub", value: "github" },
];

const STATUSES: { label: string; value: string }[] = [
  { label: "All", value: "" },
  { label: "Accepted", value: "accepted" },
  { label: "Ignored", value: "ignored" },
  { label: "Rejected", value: "rejected" },
  { label: "Error", value: "error" },
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

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm">Filters</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-4">
          <ChipRow label="Provider" current={provider} param="provider" options={PROVIDERS} />
          <ChipRow label="Status" current={status} param="status" options={STATUSES} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="pb-0">
          <CardTitle className="text-sm">
            {res.total} delivery{res.total === 1 ? "" : "ies"} total
          </CardTitle>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Received</TableHead>
                <TableHead>Provider</TableHead>
                <TableHead>Event</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>HTTP</TableHead>
                <TableHead>Error</TableHead>
                <TableHead className="text-right">#</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {res.deliveries.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={7} className="text-center text-sm text-muted-foreground py-8">
                    No deliveries match this filter.
                  </TableCell>
                </TableRow>
              ) : (
                res.deliveries.map((d) => <Row key={d.id} d={d} />)
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <div className="flex items-center justify-between">
        <span className="text-xs text-muted-foreground">
          Showing {offset + 1}–{offset + res.deliveries.length} of {res.total}
        </span>
        <div className="flex gap-2">
          <Button
            variant="outline"
            size="sm"
            nativeButton={false}
            disabled={offset === 0}
            render={
              <Link href={pageHref({ provider, status, offset: prevOffset }) as Route}>
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
              <Link href={pageHref({ provider, status, offset: offset + PAGE_SIZE }) as Route}>
                Next
              </Link>
            }
          />
        </div>
      </div>
    </div>
  );
}

function Row({ d }: { d: WebhookDeliverySummary }) {
  return (
    <TableRow className="hover:bg-muted/40">
      <TableCell className="text-xs text-muted-foreground whitespace-nowrap">
        {fmtAt(d.received_at)}
      </TableCell>
      <TableCell className="font-mono text-xs">{d.provider}</TableCell>
      <TableCell className="font-mono text-xs">{d.event}</TableCell>
      <TableCell>
        <StatusBadge status={d.status} />
      </TableCell>
      <TableCell className="font-mono text-xs">{d.http_status}</TableCell>
      <TableCell className="max-w-[280px] truncate text-xs text-muted-foreground" title={d.error}>
        {d.error ?? "—"}
      </TableCell>
      <TableCell className="text-right font-mono text-xs">{d.id}</TableCell>
    </TableRow>
  );
}

function StatusBadge({ status }: { status: WebhookDeliverySummary["status"] }) {
  const tone =
    status === "accepted"
      ? "bg-status-success-bg text-status-success-fg"
      : status === "rejected"
        ? "bg-status-failed-bg text-status-failed-fg"
        : status === "error"
          ? "bg-status-warning-bg text-status-warning-fg"
          : "bg-muted text-muted-foreground";
  return (
    <span className={cn("inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium", tone)}>
      {status}
    </span>
  );
}

function ChipRow({
  label,
  current,
  param,
  options,
}: {
  label: string;
  current: string;
  param: string;
  options: { label: string; value: string }[];
}) {
  return (
    <div className="space-y-2">
      <p className="text-xs font-medium text-muted-foreground">{label}</p>
      <div className="flex flex-wrap gap-1">
        {options.map((opt) => {
          const active = current === opt.value;
          const href =
            param === "provider"
              ? pageHref({ provider: opt.value, status: "", offset: 0 })
              : pageHref({ provider: "", status: opt.value, offset: 0 });
          return (
            <Button
              key={opt.label}
              size="sm"
              variant={active ? "default" : "outline"}
              className="h-7 px-3 text-xs"
              nativeButton={false}
              render={<Link href={href as Route}>{opt.label}</Link>}
            />
          );
        })}
      </div>
    </div>
  );
}

function pageHref({ provider, status, offset }: { provider: string; status: string; offset: number }) {
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
