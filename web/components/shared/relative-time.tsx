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
// label ship from SSR. Strategy: compute from props during render
// so prop changes show immediately, suppress the text hydration
// warning, and tick every 15s for long-lived pages.
export function RelativeTime({ at, fallback = "never", className }: Props) {
  const [, bump] = useState(0);

  useEffect(() => {
    if (!at) return;
    // The interval keeps the label live for long-lived pages
    // (dashboard, projects list) without a full refresh.
    const id = setInterval(() => bump((n) => n + 1), 15_000);
    return () => clearInterval(id);
  }, [at]);

  if (!at) return <span className={className}>{fallback}</span>;

  const label = formatRelative(at);
  const absolute = new Date(at).toLocaleString();
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <time
            dateTime={at}
            className={className}
            // The label uses Date.now(), so it can legitimately differ
            // from SSR output. Suppress only for this text node.
            suppressHydrationWarning
          />
        }
      >
        {label}
      </TooltipTrigger>
      <TooltipContent>{absolute}</TooltipContent>
    </Tooltip>
  );
}
