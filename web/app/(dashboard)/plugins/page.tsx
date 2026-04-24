import type { Metadata } from "next";
import { Package } from "lucide-react";

import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { PluginsExplorer } from "@/components/plugins/plugins-explorer.client";
import { listPlugins } from "@/server/queries/projects";

export const metadata: Metadata = {
  title: "Plugins — gocdnext",
};

// Force dynamic so a newly-shipped plugin surfaces after a server
// restart without RSC cache staleness. Catalog is tiny; no-store
// cost is negligible.
export const dynamic = "force-dynamic";

export default async function PluginsPage() {
  const { plugins } = await listPlugins();

  if (plugins.length === 0) {
    return (
      <section className="space-y-6">
        <PageHeader />
        <EmptyState />
      </section>
    );
  }

  return (
    <section className="space-y-6">
      <PageHeader />
      <p className="text-sm text-muted-foreground">
        {plugins.length} plugin{plugins.length === 1 ? "" : "s"} registered.
        Click a card to view inputs, examples, and the secrets wire-up.
        Inputs are validated against every{" "}
        <code className="rounded bg-muted px-1 py-0.5 text-xs">with:</code>{" "}
        block at apply time — a typo fails loudly instead of silently
        dropping a ghost env var at runtime.
      </p>
      <PluginsExplorer plugins={plugins} />
    </section>
  );
}

function PageHeader() {
  return (
    <header className="space-y-1">
      <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
        <Package className="h-6 w-6" aria-hidden />
        Plugins
      </h1>
    </header>
  );
}

function EmptyState() {
  return (
    <Card>
      <CardHeader className="items-center text-center">
        <div className="flex size-12 items-center justify-center rounded-full border border-dashed border-border bg-muted/30">
          <Package className="size-6 text-muted-foreground" aria-hidden />
        </div>
        <CardTitle>No plugins loaded</CardTitle>
        <CardDescription className="max-w-md">
          Set{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            GOCDNEXT_PLUGIN_CATALOG_DIR
          </code>{" "}
          to the path containing{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            &lt;name&gt;/plugin.yaml
          </code>{" "}
          manifests and restart the server. Third-party{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">uses:</code>{" "}
          images still work; they just skip schema validation.
        </CardDescription>
      </CardHeader>
    </Card>
  );
}
