"use client";

import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import { RefreshCw, Rocket } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import type { RolloutsList } from "@/types/api";

import { RolloutPanel } from "./rollout-panel";
import { RolloutsHeader } from "./rollouts-header";

const POLL_MS = 5_000;

async function fetchRollouts(
  apiBaseURL: string,
  slug: string,
  cluster: string,
  namespace: string,
): Promise<RolloutsList> {
  const base = apiBaseURL.replace(/\/+$/, "");
  const qs = new URLSearchParams({ cluster, namespace });
  const res = await fetch(
    `${base}/api/v1/projects/${encodeURIComponent(slug)}/rollouts?${qs.toString()}`,
    {
      cache: "no-store",
      credentials: "include",
      headers: { Accept: "application/json" },
    },
  );
  if (!res.ok) throw new Error(`rollouts: ${res.status}`);
  return (await res.json()) as RolloutsList;
}

type Props = {
  slug: string;
  cluster: string;
  namespace: string;
  apiBaseURL: string;
  initialData: RolloutsList;
  // canManage gates the direct Promote/Abort + gate Approve/Reject actions. The read
  // route is maintainer-gated, so a viewer never reaches this page; the flag is defence
  // in depth (and the server re-enforces every action).
  canManage: boolean;
  // focusName anchors the view on ONE rollout — the deep link from the Environments
  // card names the rollout a gate pinned. It highlights and scrolls to it but does NOT
  // filter: sibling rollouts in the namespace are context an operator may want before
  // approving, and this page carries control actions.
  focusName?: string;
};

// RolloutsLive wraps the server-fetched snapshot and re-polls it every ~5s
// (same pattern as DeployWatchesProvider), re-rendering the panels in place.
// The pulsing dot reflects the live poll; the refresh button forces an
// immediate refetch and spins while a fetch is in flight.
export function RolloutsLive({
  slug,
  cluster,
  namespace,
  apiBaseURL,
  initialData,
  canManage,
  focusName,
}: Props) {
  // Scroll the deep-linked rollout into view ONCE. Guarded by a ref so the 5s poll
  // (which re-renders on every tick) can't yank the page back under the operator while
  // they are reading a sibling panel.
  const scrolled = useRef(false);
  useEffect(() => {
    if (!focusName || scrolled.current) return;
    const el = document.getElementById(`rollout-${focusName}`);
    if (!el) return; // not in this namespace (or not loaded yet) — leave the view alone
    scrolled.current = true;
    el.scrollIntoView({ behavior: "smooth", block: "start" });
  }, [focusName]);

  const { data, isFetching, isError, refetch } = useQuery({
    queryKey: ["rollouts", slug, cluster, namespace],
    queryFn: () => fetchRollouts(apiBaseURL, slug, cluster, namespace),
    refetchInterval: POLL_MS,
    staleTime: POLL_MS,
    // The server-rendered snapshot seeds the cache; without an explicit
    // initialDataUpdatedAt react-query treats it as fresh from mount, so the
    // first poll lands ~POLL_MS later instead of firing an immediate refetch.
    initialData,
  });

  const rollouts = data?.rollouts ?? [];

  const controls = (
    <>
      <span className="inline-flex items-center gap-2 rounded-lg border border-border bg-card px-3 py-1.5 font-mono text-[11.5px] font-medium text-muted-foreground">
        <span className="relative flex size-2" aria-hidden>
          {!isError ? (
            <span className="absolute inline-flex size-full animate-ping rounded-full bg-teal-500 opacity-60" />
          ) : null}
          <span
            className={cn(
              "relative inline-flex size-2 rounded-full",
              isError ? "bg-amber-500" : "bg-teal-500",
            )}
          />
        </span>
        {isError ? "reconnecting" : "live"} · {namespace}
      </span>
      <Button
        variant="ghost"
        size="icon"
        onClick={() => refetch()}
        aria-label="Refresh rollouts"
        title="Refresh"
      >
        <RefreshCw
          className={cn("size-4", isFetching ? "animate-spin" : "")}
          aria-hidden
        />
      </Button>
    </>
  );

  return (
    <div className="space-y-6">
      <RolloutsHeader right={controls} />
      {rollouts.length === 0 ? (
        <EmptyRollouts cluster={cluster} namespace={namespace} />
      ) : (
        <div className="space-y-6">
          {rollouts.map((r) => (
            <RolloutPanel
              key={`${r.namespace}/${r.name}`}
              rollout={r}
              slug={slug}
              cluster={cluster}
              canManage={canManage}
              focused={!!focusName && r.name === focusName}
              onActed={() => refetch()}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function EmptyRollouts({
  cluster,
  namespace,
}: {
  cluster: string;
  namespace: string;
}) {
  return (
    <div className="flex flex-col items-center justify-center rounded-xl border border-dashed border-border bg-card py-16 text-center">
      <Rocket className="size-8 text-muted-foreground" aria-hidden />
      <h3 className="mt-4 text-sm font-medium">No active rollouts</h3>
      <p className="mt-1 max-w-md text-sm text-muted-foreground">
        Nothing is rolling out in{" "}
        <span className="font-mono text-foreground">{namespace}</span> on{" "}
        <span className="font-mono text-foreground">{cluster}</span> right now.
        New rollouts appear here automatically.
      </p>
    </div>
  );
}
