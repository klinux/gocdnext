"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { Rocket } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

type Props = {
  // basePath is the route without query (e.g. /projects/<slug>/rollouts); the
  // selector appends ?cluster=&namespace= and navigates.
  basePath: string;
  defaultCluster?: string;
  defaultNamespace?: string;
};

// RolloutSelector is the needs-params state: rollouts are read per cluster +
// namespace, so with neither present we ask for them instead of hard-failing.
// A native form submit (Enter) and the Load button both route to the same URL
// with the query params set, which re-runs the RSC fetch.
export function RolloutSelector({
  basePath,
  defaultCluster = "",
  defaultNamespace = "",
}: Props) {
  const router = useRouter();
  const [cluster, setCluster] = useState(defaultCluster);
  const [namespace, setNamespace] = useState(defaultNamespace);
  const ready = cluster.trim() !== "" && namespace.trim() !== "";

  function load() {
    if (!ready) return;
    const qs = new URLSearchParams({
      cluster: cluster.trim(),
      namespace: namespace.trim(),
    });
    router.push(`${basePath}?${qs.toString()}`);
  }

  return (
    <div className="rounded-xl border border-dashed border-border bg-card p-6">
      <div className="flex items-center gap-2 text-sm font-medium">
        <Rocket className="size-4 text-teal-500" aria-hidden />
        Pick a cluster and namespace
      </div>
      <p className="mt-1 max-w-2xl text-sm text-muted-foreground">
        Rollouts are read per Kubernetes cluster and namespace. Enter the
        ArgoCD-registered cluster and the namespace of the application to load
        its in-flight canary and blue-green rollouts.
      </p>
      <form
        className="mt-4 flex flex-wrap items-end gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          load();
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
        <Button type="button" onClick={load} disabled={!ready}>
          Load
        </Button>
      </form>
    </div>
  );
}
