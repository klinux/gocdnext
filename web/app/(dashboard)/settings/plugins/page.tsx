import type { Metadata } from "next";
import {
  Box,
  Container,
  FileCode,
  Package,
  PackageOpen,
  ShieldCheck,
  Ship,
} from "lucide-react";
import type { LucideIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
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
        {plugins.length > 0 ? (
          <p className="text-xs text-muted-foreground">
            {plugins.length} plugin{plugins.length === 1 ? "" : "s"} registered
            · jump to:{" "}
            {plugins.map((p, i) => (
              <span key={p.name}>
                <a
                  href={`#plugin-${p.name}`}
                  className="font-mono underline-offset-4 hover:underline"
                >
                  {p.name}
                </a>
                {i < plugins.length - 1 ? ", " : ""}
              </span>
            ))}
          </p>
        ) : null}
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

// pluginIcon maps a plugin name to a semantically-fitting Lucide
// icon so the card header scans at a glance — package (generic
// build), container (docker), ship (kubectl), shield (trivy),
// file-code (slack webhook / notifications), etc. Unknown names
// fall back to the default plug icon.
function pluginIcon(name: string): LucideIcon {
  switch (name) {
    case "docker":
      return Container;
    case "kubectl":
      return Ship;
    case "trivy":
      return ShieldCheck;
    case "slack":
      return FileCode;
    case "node":
    case "go":
      return Box;
    default:
      return PackageOpen;
  }
}

function PluginCard({ plugin }: { plugin: PluginSummary }) {
  const Icon = pluginIcon(plugin.name);
  const required = plugin.inputs.filter((i) => i.required);
  const optional = plugin.inputs.filter((i) => !i.required);

  return (
    <Card id={`plugin-${plugin.name}`} className="scroll-mt-20 overflow-hidden">
      <CardHeader className="flex flex-row items-start gap-4 space-y-0">
        <div
          className="flex size-10 shrink-0 items-center justify-center rounded-md border border-border bg-muted/50"
          aria-hidden
        >
          <Icon className="size-5 text-muted-foreground" />
        </div>
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle className="font-mono text-base">
              gocdnext/{plugin.name}
            </CardTitle>
            <Badge variant="outline" className="font-normal">
              plugin
            </Badge>
            <Badge variant="secondary" className="font-normal">
              {plugin.inputs.length}{" "}
              {plugin.inputs.length === 1 ? "input" : "inputs"}
            </Badge>
          </div>
          {plugin.description ? (
            <CardDescription className="leading-relaxed">
              {plugin.description}
            </CardDescription>
          ) : null}
        </div>
      </CardHeader>

      <CardContent className="space-y-6">
        <UsageHint name={plugin.name} required={required} />

        {plugin.inputs.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No inputs declared — the plugin reads everything from the
            workspace and its own image defaults.
          </p>
        ) : (
          <div className="space-y-4">
            {required.length > 0 ? (
              <InputsSection
                label="Required"
                tone="required"
                inputs={required}
              />
            ) : null}
            {optional.length > 0 ? (
              <InputsSection
                label="Optional"
                tone="optional"
                inputs={optional}
              />
            ) : null}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function InputsSection({
  label,
  tone,
  inputs,
}: {
  label: string;
  tone: "required" | "optional";
  inputs: PluginSummary["inputs"];
}) {
  return (
    <section className="space-y-2">
      <h4 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
        {label} ({inputs.length})
      </h4>
      <div className="divide-y divide-border overflow-hidden rounded-md border border-border">
        {inputs.map((i) => (
          <InputRow key={i.name} input={i} tone={tone} />
        ))}
      </div>
    </section>
  );
}

function InputRow({
  input,
  tone,
}: {
  input: PluginSummary["inputs"][number];
  tone: "required" | "optional";
}) {
  return (
    <div className="space-y-1 px-3 py-2.5">
      <div className="flex flex-wrap items-center gap-2">
        <code className="font-mono text-sm font-semibold">{input.name}</code>
        {tone === "required" ? (
          <span className="inline-flex items-center rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-amber-700 dark:text-amber-400">
            required
          </span>
        ) : (
          <span className="inline-flex items-center rounded-full border border-border bg-muted/40 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
            optional
          </span>
        )}
        {input.default ? (
          <span className="text-xs text-muted-foreground">
            default{" "}
            <code className="rounded bg-muted px-1 py-0.5 font-mono">
              {input.default}
            </code>
          </span>
        ) : null}
      </div>
      {input.description ? (
        <p className="text-sm leading-relaxed text-muted-foreground">
          {input.description}
        </p>
      ) : null}
    </div>
  );
}

// UsageHint renders a minimal example YAML block per plugin so
// the operator doesn't have to cross-reference the inputs list
// with the YAML shape mentally. Only required inputs get filled
// (as `<value>`) so the hint stays honest — we don't invent
// defaults that aren't the plugin's actual default.
function UsageHint({
  name,
  required,
}: {
  name: string;
  required: PluginSummary["inputs"];
}) {
  const lines: string[] = [
    "jobs:",
    "  example:",
    "    stage: build",
    `    uses: gocdnext/${name}@v1`,
  ];
  if (required.length > 0) {
    lines.push("    with:");
    for (const r of required) {
      lines.push(`      ${r.name}: <${r.name}>`);
    }
  }
  return (
    <div className="space-y-2">
      <h4 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
        Usage
      </h4>
      <pre className="overflow-x-auto rounded-md border border-border bg-muted/30 px-3 py-2 text-xs leading-relaxed">
        <code className="font-mono">{lines.join("\n")}</code>
      </pre>
    </div>
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
