"use client";

import { createContext, useContext, useMemo, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";

import type { DeployWatch, DeployWatchesList } from "@/types/api";

const POLL_MS = 5_000;

// Empty by default so a card rendered without the provider (e.g. in unit tests) just
// shows no live chip.
const DeployWatchesContext = createContext<Map<string, DeployWatch>>(new Map());

async function fetchDeployWatches(
  apiBaseURL: string,
  slug: string,
): Promise<DeployWatchesList> {
  const base = apiBaseURL.replace(/\/+$/, "");
  const res = await fetch(
    `${base}/api/v1/projects/${encodeURIComponent(slug)}/deploy-watches`,
    { cache: "no-store", credentials: "include", headers: { Accept: "application/json" } },
  );
  if (!res.ok) throw new Error(`deploy-watches: ${res.status}`);
  return (await res.json()) as DeployWatchesList;
}

// DeployWatchesProvider polls the project's in-flight native deploys and exposes them
// keyed by environment name. One poll per project (not per card). The children are
// RSC-composed cards that read their env's live state via useDeployWatch.
export function DeployWatchesProvider({
  slug,
  apiBaseURL,
  children,
}: {
  slug: string;
  apiBaseURL: string;
  children: ReactNode;
}) {
  const { data } = useQuery({
    queryKey: ["deploy-watches", slug],
    queryFn: () => fetchDeployWatches(apiBaseURL, slug),
    refetchInterval: POLL_MS,
    staleTime: POLL_MS,
  });

  const byEnv = useMemo(
    () => new Map((data?.deploy_watches ?? []).map((w) => [w.environment, w])),
    [data],
  );

  return (
    <DeployWatchesContext.Provider value={byEnv}>
      {children}
    </DeployWatchesContext.Provider>
  );
}

// useDeployWatch returns the in-flight native deploy for an environment, or undefined
// when none is active (or the provider isn't mounted).
export function useDeployWatch(environment: string): DeployWatch | undefined {
  return useContext(DeployWatchesContext).get(environment);
}
