"use client";

import { useMemo, useRef, useState } from "react";
import { ChevronDown, Search } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { DeployWatchesProvider } from "@/components/environments/deploy-watches-provider.client";
import {
  EnvironmentCard,
  type EnvironmentCardHandle,
} from "@/components/environments/environment-card.client";
import { cn } from "@/lib/utils";
import type {
  DeployTarget,
  EnvironmentSummary,
} from "@/types/api";

type Filter = "all" | "active" | "frozen" | "empty";

type Props = {
  slug: string;
  environments: EnvironmentSummary[];
  targets: DeployTarget[];
  canManage: boolean;
  isAdmin: boolean;
  apiBaseURL: string;
};

const FILTERS: Array<{ key: Filter; label: string }> = [
  { key: "all", label: "All" },
  { key: "active", label: "Active" },
  { key: "frozen", label: "Frozen" },
  { key: "empty", label: "No deploy" },
];

function envFilter(environment: EnvironmentSummary): Exclude<Filter, "all"> {
  if (environment.frozen) return "frozen";
  if (!environment.current) return "empty";
  return "active";
}

function fieldMatches(value: string | undefined, query: string): boolean {
  return value?.toLowerCase().includes(query) ?? false;
}

function matchesQuery(
  environment: EnvironmentSummary,
  target: DeployTarget | undefined,
  query: string,
): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  return (
    fieldMatches(environment.name, q) ||
    fieldMatches(environment.description, q) ||
    fieldMatches(environment.current?.version, q) ||
    fieldMatches(target?.provider, q) ||
    fieldMatches(target?.cluster, q) ||
    fieldMatches(target?.application, q) ||
    fieldMatches(target?.namespace, q) ||
    fieldMatches(target?.sync_mode, q)
  );
}

// EnvironmentsExplorer owns the client-only toolbar for the project environments
// page. The RSC page still does every fetch once; this wrapper filters the small
// in-memory list and controls "expand histories" without adding an endpoint.
export function EnvironmentsExplorer({
  slug,
  environments,
  targets,
  canManage,
  isAdmin,
  apiBaseURL,
}: Props) {
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [historyOpen, setHistoryOpen] = useState<Record<string, boolean>>({});
  const cardRefs = useRef(new Map<string, EnvironmentCardHandle>());

  const targetByEnv = useMemo(
    () => new Map(targets.map((target) => [target.environment, target])),
    [targets],
  );

  const counts = useMemo(() => {
    const next: Record<Filter, number> = {
      all: environments.length,
      active: 0,
      frozen: 0,
      empty: 0,
    };
    for (const environment of environments) next[envFilter(environment)] += 1;
    return next;
  }, [environments]);

  const filtered = useMemo(() => {
    return environments.filter((environment) => {
      if (filter !== "all" && envFilter(environment) !== filter) return false;
      return matchesQuery(environment, targetByEnv.get(environment.name), query);
    });
  }, [environments, filter, query, targetByEnv]);

  const visibleHistoryEnvironments = filtered.filter(
    (environment) =>
      environment.has_environment_row && environment.id !== undefined,
  );
  const allVisibleHistoriesOpen =
    visibleHistoryEnvironments.length > 0 &&
    visibleHistoryEnvironments.every(
      (environment) => historyOpen[environment.name],
    );

  function toggleVisibleHistories() {
    const nextOpen = !allVisibleHistoriesOpen;
    setHistoryOpen((prev) => {
      const next = { ...prev };
      for (const environment of visibleHistoryEnvironments) {
        next[environment.name] = nextOpen;
      }
      return next;
    });
    for (const environment of visibleHistoryEnvironments) {
      cardRefs.current.get(environment.name)?.setHistoryOpen(nextOpen);
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2.5">
        <div className="relative w-full min-w-[230px] shrink-0 sm:w-[260px] lg:w-[300px]">
          <Search
            className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            value={query}
            onValueChange={(next: string) => setQuery(next)}
            placeholder="Filter environment, version, or target..."
            aria-label="Search environments"
            className="h-9 pl-8 text-sm"
          />
        </div>

        <div
          role="group"
          className="flex shrink-0 rounded-lg border border-border bg-card p-1"
          aria-label="Environment status filter"
        >
          {FILTERS.map((item) => (
            <button
              key={item.key}
              type="button"
              aria-label={`${item.label} (${counts[item.key]})`}
              aria-pressed={filter === item.key}
              onClick={() => setFilter(item.key)}
              className={cn(
                "rounded-md px-2.5 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-muted hover:text-foreground",
                filter === item.key && "bg-muted text-foreground",
              )}
            >
              {item.label}
              <span
                aria-hidden="true"
                className="ml-1 rounded-full bg-background/70 px-1.5 text-[10px] tabular-nums"
              >
                {counts[item.key]}
              </span>
            </button>
          ))}
        </div>

        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="shrink-0 text-xs text-muted-foreground"
          onClick={toggleVisibleHistories}
          disabled={visibleHistoryEnvironments.length === 0}
        >
          <ChevronDown
            className={cn(
              "size-3.5 transition-transform",
              allVisibleHistoriesOpen && "rotate-180",
            )}
            aria-hidden
          />
          {allVisibleHistoriesOpen ? "Collapse histories" : "Expand histories"}
        </Button>

        <span className="ml-auto text-xs text-muted-foreground tabular-nums">
          {filtered.length} of {environments.length} environments
        </span>
      </div>

      {filtered.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border bg-card py-10 text-center text-sm text-muted-foreground">
          No environments match the current filters.
        </div>
      ) : (
        <DeployWatchesProvider slug={slug} apiBaseURL={apiBaseURL}>
          <div className="grid grid-cols-1 items-start gap-4 min-[760px]:grid-cols-[repeat(auto-fill,minmax(400px,1fr))]">
            {filtered.map((environment) => (
              <EnvironmentCard
                ref={(node) => {
                  if (node) {
                    cardRefs.current.set(environment.name, node);
                  } else {
                    cardRefs.current.delete(environment.name);
                  }
                }}
                // Keyed by NAME, never by id: `id` is absent on a freeze-only
                // row (#202), so two orphan freezes would share an `undefined`
                // key and React would carry one card's local state (open
                // history, pending state) onto the other after an unfreeze.
                // Name is unique per project by construction.
                key={environment.name}
                slug={slug}
                environment={environment}
                deployTarget={targetByEnv.get(environment.name)}
                canManage={canManage}
                isAdmin={isAdmin}
                apiBaseURL={apiBaseURL}
                onHistoryOpenChange={(open) =>
                  setHistoryOpen((prev) => ({
                    ...prev,
                    [environment.name]: open,
                  }))
                }
              />
            ))}
          </div>
        </DeployWatchesProvider>
      )}
    </div>
  );
}
