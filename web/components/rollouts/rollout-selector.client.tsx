"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import { ChevronDown, Rocket } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import type { RolloutPick } from "@/lib/rollouts";

type Props = {
  // basePath is the route without query (e.g. /projects/<slug>/rollouts); the
  // selector appends ?cluster=&namespace= and navigates.
  basePath: string;
  // picks are the project's configured rollout targets, offered as one-click
  // entries so the user never has to guess cluster/namespace names.
  picks?: RolloutPick[];
  defaultCluster?: string;
  defaultNamespace?: string;
};

// RolloutSelector is the needs-params state: rollouts are read per cluster +
// namespace, so with neither present we ask for them instead of hard-failing.
// The primary path is one-click quick-picks derived from the project's
// configured rollout targets; manual entry stays available (collapsed when
// there are picks) for an ad-hoc cluster/namespace. Any choice routes to the
// same URL with the query params set, which re-runs the RSC fetch.
export function RolloutSelector({
  basePath,
  picks = [],
  defaultCluster = "",
  defaultNamespace = "",
}: Props) {
  const router = useRouter();
  const [cluster, setCluster] = useState(defaultCluster);
  const [namespace, setNamespace] = useState(defaultNamespace);
  // Manual entry is expanded by default only when there's nothing to pick.
  const [manual, setManual] = useState(picks.length === 0);
  const ready = cluster.trim() !== "" && namespace.trim() !== "";

  function go(c: string, ns: string) {
    const qs = new URLSearchParams({ cluster: c.trim(), namespace: ns.trim() });
    // typedRoutes can't prove a runtime-built query string is a known route — cast, the
    // same pattern every other dynamic router.push in the app uses.
    router.push(`${basePath}?${qs.toString()}` as Route);
  }

  return (
    <div className="rounded-xl border border-dashed border-border bg-card p-6">
      <div className="flex items-center gap-2 text-sm font-medium">
        <Rocket className="size-4 text-teal-500" aria-hidden />
        {picks.length > 0 ? "Pick a rollout" : "Pick a cluster and namespace"}
      </div>
      <p className="mt-1 max-w-2xl text-sm text-muted-foreground">
        Rollouts are read per Kubernetes cluster and namespace.{" "}
        {picks.length > 0
          ? "Choose one of this project's configured rollout targets, or enter a cluster and namespace manually."
          : "Enter the ArgoCD-registered cluster and the namespace of the application to load its in-flight canary and blue-green rollouts."}
      </p>

      {picks.length > 0 ? (
        <ul className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {picks.map((p) => (
            <li key={`${p.cluster} ${p.namespace}`}>
              <button
                type="button"
                onClick={() => go(p.cluster, p.namespace)}
                className={cn(
                  "group flex w-full flex-col items-start gap-1 rounded-lg border border-border",
                  "bg-background p-4 text-left transition-colors",
                  "hover:border-teal-500/60 hover:bg-teal-500/5",
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-teal-500/50",
                )}
              >
                <span className="flex items-center gap-2 text-sm font-medium">
                  <Rocket
                    className="size-4 text-teal-500 transition-transform group-hover:scale-110"
                    aria-hidden
                  />
                  {p.environment}
                </span>
                {p.rolloutName ? (
                  <span className="text-xs text-muted-foreground">
                    {p.rolloutName}
                  </span>
                ) : null}
                <span className="mt-1 font-mono text-xs text-muted-foreground">
                  {p.cluster} · {p.namespace}
                </span>
              </button>
            </li>
          ))}
        </ul>
      ) : null}

      {picks.length > 0 && !manual ? (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="mt-3 h-8 text-xs text-muted-foreground"
          onClick={() => setManual(true)}
        >
          <ChevronDown className="mr-1 size-3.5" aria-hidden />
          Or enter a cluster and namespace manually
        </Button>
      ) : null}

      {manual ? (
        <form
          className={cn(
            "flex flex-wrap items-end gap-3",
            picks.length > 0 ? "mt-4 border-t border-border pt-4" : "mt-4",
          )}
          onSubmit={(e) => {
            e.preventDefault();
            if (ready) go(cluster, namespace);
          }}
        >
          <div className="grid gap-1.5">
            <Label htmlFor="rollout-cluster">Cluster</Label>
            <Input
              id="rollout-cluster"
              name="cluster"
              value={cluster}
              onValueChange={setCluster}
              placeholder="prod-hub"
              className="w-48"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="rollout-namespace">Namespace</Label>
            <Input
              id="rollout-namespace"
              name="namespace"
              value={namespace}
              onValueChange={setNamespace}
              placeholder="production"
              className="w-48"
            />
          </div>
          <Button
            type="button"
            onClick={() => ready && go(cluster, namespace)}
            disabled={!ready}
          >
            Load
          </Button>
        </form>
      ) : null}
    </div>
  );
}
