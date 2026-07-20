"use client";

import { AlertTriangle, RefreshCw } from "lucide-react";

import { RelativeTime } from "@/components/shared/relative-time";
import type { DeployWatch } from "@/types/api";

function shortRev(rev: string): string {
  return rev.length > 7 ? rev.slice(0, 7) : rev;
}

// NativeWatchChip renders the live state of an in-flight native deploy, derived from
// the watch's raw fields: Degraded (persisting past the debounce window is what
// eventually fails it) wins over Syncing; before the sync fires it reads "Deploying".
export function NativeWatchChip({ watch }: { watch: DeployWatch }) {
  if (watch.degraded_since) {
    return (
      <span className="inline-flex items-center gap-1 rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-xs font-medium text-amber-600 dark:text-amber-400">
        <AlertTriangle className="size-3" aria-hidden />
        Degraded <RelativeTime at={watch.degraded_since} />
      </span>
    );
  }
  const rev = shortRev(watch.expected_revision);
  return (
    <span className="inline-flex items-center gap-1 rounded-full border border-sky-500/40 bg-sky-500/10 px-2 py-0.5 text-xs font-medium text-sky-600 dark:text-sky-400">
      <RefreshCw className="size-3 animate-spin" aria-hidden />
      {watch.sync_requested_at ? "Syncing" : "Deploying"}
      {rev ? <span className="font-mono font-normal">{rev}</span> : null}
    </span>
  );
}
