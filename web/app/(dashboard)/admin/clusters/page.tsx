import type { Metadata } from "next";

import { ClustersManager } from "@/components/clusters/clusters-manager.client";
import { listAdminClusters } from "@/server/queries/admin";
import { listProjects } from "@/server/queries/projects";

export const metadata: Metadata = {
  title: "Kubernetes clusters — gocdnext",
};

// Cluster mutations from this tab revalidate via the action; the extra
// force-dynamic keeps multi-tab edits in sync without a cache dance.
// Payload is small (a handful of clusters, at most).
export const dynamic = "force-dynamic";

export default async function ClustersPage() {
  // listProjects fails open: the allowed-projects picker is a
  // convenience, not a gate (the server is authoritative on the
  // allow-list). A hiccup here just means the picker shows raw IDs
  // rather than friendly names — the editor still works.
  const [clusters, projects] = await Promise.all([
    listAdminClusters(),
    listProjects().catch(() => []),
  ]);
  const projectOptions = projects
    .map((p) => ({ id: p.id, name: p.name, slug: p.slug }))
    .sort((a, b) => a.name.localeCompare(b.name));

  return (
    <section className="space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">
          Kubernetes clusters
        </h2>
        <p className="text-sm text-muted-foreground">
          Registered clusters agents can target to run jobs. Each entry holds a
          credential (kubeconfig, bearer token, or in-cluster ServiceAccount)
          and an optional allow-list of projects permitted to use it.
        </p>
      </div>

      <ClustersManager initial={clusters} projects={projectOptions} />
    </section>
  );
}
