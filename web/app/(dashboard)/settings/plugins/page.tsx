import type { Metadata } from "next";
import { Package } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { listPlugins } from "@/server/queries/projects";
import type { PluginSummary } from "@/types/api";

export const metadata: Metadata = {
  title: "Settings — Plugins",
};

// Forcing dynamic so a newly-shipped plugin shows up after a
// server restart without a stale render sitting behind Next's
// default RSC cache. The catalog is small; a no-store fetch is
// cheap enough that operators appreciate the freshness.
export const dynamic = "force-dynamic";

export default async function PluginsPage() {
  const { plugins } = await listPlugins();

  return (
    <section className="space-y-6">
      <header className="space-y-1">
        <p className="text-sm text-muted-foreground">
          Plugins registered in the server&apos;s catalog. Each one maps to a
          container image invoked via{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">uses:</code>{" "}
          in a job. Inputs below are validated against every{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">with:</code>{" "}
          block at apply time — a typo fails loudly instead of silently
          dropping a ghost env var at runtime.
        </p>
      </header>

      {plugins.length === 0 ? (
        <EmptyState />
      ) : (
        <div className="space-y-4">
          {plugins.map((p) => (
            <PluginCard key={p.name} plugin={p} />
          ))}
        </div>
      )}
    </section>
  );
}

function PluginCard({ plugin }: { plugin: PluginSummary }) {
  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-center gap-2">
          <CardTitle className="font-mono text-base">
            gocdnext/{plugin.name}
          </CardTitle>
          <Badge variant="outline">plugin</Badge>
        </div>
        {plugin.description ? (
          <CardDescription>{plugin.description}</CardDescription>
        ) : null}
      </CardHeader>
      <CardContent>
        <UsageHint name={plugin.name} inputs={plugin.inputs} />
        {plugin.inputs.length === 0 ? (
          <p className="mt-4 text-sm text-muted-foreground">
            No inputs declared. The plugin reads everything it needs from the
            workspace and its own image defaults.
          </p>
        ) : (
          <Table className="mt-4">
            <TableHeader>
              <TableRow>
                <TableHead className="w-1/4">Input</TableHead>
                <TableHead className="w-1/6">Required</TableHead>
                <TableHead className="w-1/6">Default</TableHead>
                <TableHead>Description</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {plugin.inputs.map((i) => (
                <TableRow key={i.name}>
                  <TableCell className="font-mono">{i.name}</TableCell>
                  <TableCell>
                    {i.required ? (
                      <Badge variant="destructive">required</Badge>
                    ) : (
                      <span className="text-xs text-muted-foreground">
                        optional
                      </span>
                    )}
                  </TableCell>
                  <TableCell className="font-mono text-muted-foreground">
                    {i.default ? i.default : "—"}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {i.description ?? ""}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

// UsageHint renders a minimal example YAML block per plugin so
// the operator doesn't have to cross-reference the inputs table
// with the YAML shape mentally. Only required inputs get filled
// (as `<value>`) so the hint stays honest — we don't invent
// defaults that aren't the plugin's actual default.
function UsageHint({
  name,
  inputs,
}: {
  name: string;
  inputs: PluginSummary["inputs"];
}) {
  const required = inputs.filter((i) => i.required);
  return (
    <pre className="overflow-x-auto rounded-md border border-border bg-muted/50 px-3 py-2 text-xs">
      <code>
        {`jobs:\n  example:\n    stage: build\n    uses: gocdnext/${name}@v1`}
        {required.length > 0 ? "\n    with:\n" : "\n"}
        {required
          .map((i) => `      ${i.name}: <${i.name}>`)
          .join("\n")}
      </code>
    </pre>
  );
}

function EmptyState() {
  return (
    <Card>
      <CardHeader className="text-center">
        <div className="mx-auto">
          <Package className="h-6 w-6 text-muted-foreground" aria-hidden />
        </div>
        <CardTitle>No plugins loaded</CardTitle>
        <CardDescription>
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
