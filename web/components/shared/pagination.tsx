import Link from "next/link";
import type { Route } from "next";
import { ChevronLeft, ChevronRight } from "lucide-react";

import { Button } from "@/components/ui/button";

type Props = {
  // Zero-based offset of the current window. Callers typically
  // pull this from ?offset= in searchParams.
  offset: number;
  // Total number of rows the server reports (data.total from
  // any /api/v1/...?limit=&offset= response).
  total: number;
  // Page size used for both the "showing" label and the next/prev
  // offset math. Keep in sync with the server-side limit.
  pageSize: number;
  // Base URL of the paginated page (e.g. "/runs" or
  // "/projects/demo/runs"). Query params are appended by the
  // component.
  basePath: string;
  // Extra query-string params to preserve across pagination clicks
  // (filter chips, search terms, etc.). Undefined / empty values
  // are omitted.
  params?: Record<string, string | undefined>;
};

// Pagination is the one-off inline control from the original
// /runs page pulled into a shared component so every list view
// reads from the same Prev/Next shape. Returns null when there's
// only one page (offset==0 && total<=pageSize) — no chrome on a
// short list. Keep the button + disabled state logic identical
// to the shadcn Button surface elsewhere (nativeButton=false +
// render=<Link>) so hover/focus rings match.
export function Pagination({
  offset,
  total,
  pageSize,
  basePath,
  params,
}: Props) {
  const hasPrev = offset > 0;
  const hasNext = offset + pageSize < total;
  if (!hasPrev && !hasNext) return null;

  const prev = Math.max(0, offset - pageSize);
  const next = offset + pageSize;

  return (
    <div className="flex items-center justify-between">
      <p className="text-xs text-muted-foreground tabular-nums">
        Showing {offset + 1}–{Math.min(offset + pageSize, total)} of{" "}
        {total.toLocaleString()}
      </p>
      <div className="flex gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={!hasPrev}
          nativeButton={false}
          render={
            hasPrev ? (
              <Link
                href={buildHref(basePath, { ...params, offset: String(prev) })}
              >
                <ChevronLeft className="h-3.5 w-3.5" />
                Prev
              </Link>
            ) : (
              <span>
                <ChevronLeft className="h-3.5 w-3.5" />
                Prev
              </span>
            )
          }
        />
        <Button
          variant="outline"
          size="sm"
          disabled={!hasNext}
          nativeButton={false}
          render={
            hasNext ? (
              <Link
                href={buildHref(basePath, { ...params, offset: String(next) })}
              >
                Next
                <ChevronRight className="h-3.5 w-3.5" />
              </Link>
            ) : (
              <span>
                Next
                <ChevronRight className="h-3.5 w-3.5" />
              </span>
            )
          }
        />
      </div>
    </div>
  );
}

// buildHref preserves every non-empty param in the query string.
// Returns a typed Route so Next's typedRoutes stays happy without
// each caller casting at the call site.
function buildHref(
  basePath: string,
  params: Record<string, string | undefined>,
): Route {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v != null && v !== "") q.set(k, v);
  }
  const s = q.toString();
  return (s ? `${basePath}?${s}` : basePath) as Route;
}
