"use client";

import { useEffect, useState } from "react";

import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { formatRelative } from "@/lib/format";

type Props = {
  at?: string | null;
  fallback?: string;
  className?: string;
};

// RelativeTime renders "N seconds/minutes/hours ago". Time moves
// between the server render and the client hydration (even by a
// single second), which breaks hydration if we let the relative
// label ship from SSR. Strategy: compute on the server once for
// the initial paint, then let the client take over with
// useEffect + an interval so the label stays fresh.
export function RelativeTime({ at, fallback = "never", className }: Props) {
  if (!at) return <span className={className}>{fallback}</span>;

  const server = formatRelative(at);
  const [label, setLabel] = useState(server);
  const [mounted, setMounted] = useState(false);

  useEffect(() => {
    // Recompute on mount so any server/client drift is flushed
    // before the label becomes visible. The interval keeps the
    // label live for long-lived pages (dashboard, projects list)
    // without a full refresh.
    setLabel(formatRelative(at));
    setMounted(true);
    const id = setInterval(() => setLabel(formatRelative(at)), 15_000);
    return () => clearInterval(id);
  }, [at]);

  const absolute = new Date(at).toLocaleString();
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <time
            dateTime={at}
            className={className}
            // Belt-and-suspenders: even though the mounted flag means
            // the first client paint matches the server output, the
            // 15s-interval refresh can still show a diff vs. a stale
            // SSR HTML in cached views. Suppress only for the text.
            suppressHydrationWarning
          />
        }
      >
        {mounted ? label : server}
      </TooltipTrigger>
      <TooltipContent>{absolute}</TooltipContent>
    </Tooltip>
  );
}
