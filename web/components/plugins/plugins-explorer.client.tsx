"use client";

import { useMemo, useState } from "react";
import {
  Box,
  ClipboardList,
  Container,
  FileCode,
  KeyRound,
  Package,
  PackageOpen,
  Rocket,
  Search,
  ShieldCheck,
  Ship,
  X,
  type LucideIcon,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { cn } from "@/lib/utils";
import type { PluginExample, PluginSummary } from "@/types/api";

type Props = {
  plugins: PluginSummary[];
};

// Category order also drives the filter button order. "All" is the
// default; others appear in a fixed sequence rather than alpha so
// related buckets sit next to each other (build → container →
// deploy is the natural CI pipeline flow).
const CATEGORY_ORDER = [
  "build",
  "container",
  "security",
  "deploy",
  "registry",
  "release",
  "notifications",
  "quality",
] as const;

// iconFor maps a plugin name to a semantically-fitting Lucide
// icon. Unknown names fall back to the generic plug icon.
function iconFor(name: string): LucideIcon {
  switch (name) {
    case "docker":
    case "kaniko":
      return Container;
    case "kubectl":
    case "helm":
      return Ship;
    case "trivy":
    case "gitleaks":
      return ShieldCheck;
    case "slack":
    case "discord":
      return FileCode;
    case "node":
    case "go":
    case "maven":
    case "gradle":
      return Box;
    case "github-release":
      return Rocket;
    default:
      return PackageOpen;
  }
}

// categoryTone maps each group to a tailwind tone so the filter
// buttons + badges stay visually grouped without needing a
// hand-picked color per plugin. Kept in sync with the catalog
// CATEGORY_ORDER above.
const CATEGORY_TONE: Record<string, string> = {
  build: "bg-sky-500/10 text-sky-700 border-sky-500/30 dark:text-sky-400",
  container:
    "bg-purple-500/10 text-purple-700 border-purple-500/30 dark:text-purple-400",
  security:
    "bg-red-500/10 text-red-700 border-red-500/30 dark:text-red-400",
  deploy:
    "bg-emerald-500/10 text-emerald-700 border-emerald-500/30 dark:text-emerald-400",
  registry:
    "bg-blue-500/10 text-blue-700 border-blue-500/30 dark:text-blue-400",
  release:
    "bg-indigo-500/10 text-indigo-700 border-indigo-500/30 dark:text-indigo-400",
  notifications:
    "bg-violet-500/10 text-violet-700 border-violet-500/30 dark:text-violet-400",
  quality:
    "bg-pink-500/10 text-pink-700 border-pink-500/30 dark:text-pink-400",
};

export function PluginsExplorer({ plugins }: Props) {
  const [q, setQ] = useState("");
  const [cat, setCat] = useState<string>("all");
  const [selected, setSelected] = useState<PluginSummary | null>(null);

  // Categories present in the loaded catalog, ordered by the
  // canonical sequence + "+ anything the catalog has that isn't
  // in the order list" tail so an unexpected bucket doesn't get
  // hidden from the filter bar.
  const categories = useMemo(() => {
    const seen = new Set<string>();
    for (const p of plugins) {
      if (p.category) seen.add(p.category);
    }
    const ordered = CATEGORY_ORDER.filter((c) => seen.has(c));
    const extras = Array.from(seen).filter(
      (c) => !CATEGORY_ORDER.includes(c as (typeof CATEGORY_ORDER)[number]),
    );
    return [...ordered, ...extras.sort()];
  }, [plugins]);

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return plugins.filter((p) => {
      if (cat !== "all" && p.category !== cat) return false;
      if (!needle) return true;
      // Match on name, description, and input names — operators
      // searching for "secret" or "kubeconfig" should find plugins
      // exposing those inputs even when the description doesn't
      // repeat the word.
      if (p.name.toLowerCase().includes(needle)) return true;
      if (p.description?.toLowerCase().includes(needle)) return true;
      return p.inputs.some((i) =>
        i.name.toLowerCase().includes(needle),
      );
    });
  }, [plugins, q, cat]);

  return (
    <div className="space-y-6">
      {/* Search + filter bar. Search is always visible; the
          filter chips sit on the right on wide screens and wrap
          below on mobile. */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
        <div className="relative flex-1">
          <Search
            className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            type="search"
            placeholder="Search plugins by name, description, or input…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            className="pl-9"
          />
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <FilterChip
          label="All"
          count={plugins.length}
          active={cat === "all"}
          onClick={() => setCat("all")}
          tone={null}
        />
        {categories.map((c) => {
          const count = plugins.filter((p) => p.category === c).length;
          return (
            <FilterChip
              key={c}
              label={c}
              count={count}
              active={cat === c}
              onClick={() => setCat(c)}
              tone={CATEGORY_TONE[c] ?? null}
            />
          );
        })}
      </div>

      {filtered.length === 0 ? (
        <EmptyResult onClear={() => {
          setQ("");
          setCat("all");
        }} />
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {filtered.map((p) => (
            <PluginCard
              key={p.name}
              plugin={p}
              onOpen={() => setSelected(p)}
            />
          ))}
        </div>
      )}

      <PluginSheet
        plugin={selected}
        open={selected !== null}
        onOpenChange={(open) => {
          if (!open) setSelected(null);
        }}
      />
    </div>
  );
}

function FilterChip({
  label,
  count,
  active,
  onClick,
  tone,
}: {
  label: string;
  count: number;
  active: boolean;
  onClick: () => void;
  tone: string | null;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-medium transition",
        active
          ? tone ??
              "border-primary bg-primary text-primary-foreground"
          : "border-border bg-muted/30 text-muted-foreground hover:text-foreground",
        active && tone ? "ring-2 ring-offset-1 ring-current/30" : "",
      )}
    >
      <span className="capitalize">{label}</span>
      <span
        className={cn(
          "rounded-full px-1.5 text-[10px] font-semibold",
          active ? "bg-background/30" : "bg-background/60",
        )}
      >
        {count}
      </span>
    </button>
  );
}

function PluginCard({
  plugin,
  onOpen,
}: {
  plugin: PluginSummary;
  onOpen: () => void;
}) {
  const Icon = iconFor(plugin.name);
  const tone = plugin.category ? CATEGORY_TONE[plugin.category] : null;

  return (
    <button
      type="button"
      onClick={onOpen}
      aria-label={`Open details for gocdnext/${plugin.name}`}
      className="text-left"
    >
      <Card className="h-full cursor-pointer transition hover:border-foreground/20 hover:shadow-md">
        <CardHeader className="flex flex-row items-start gap-3 space-y-0">
          <div
            className={cn(
              "flex size-10 shrink-0 items-center justify-center rounded-md border",
              tone ?? "border-border bg-muted/50",
            )}
            aria-hidden
          >
            <Icon className="size-5" />
          </div>
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <CardTitle className="truncate font-mono text-sm">
                gocdnext/{plugin.name}
              </CardTitle>
            </div>
            {plugin.category ? (
              <Badge
                variant="outline"
                className={cn("mt-1 font-normal capitalize", tone)}
              >
                {plugin.category}
              </Badge>
            ) : null}
          </div>
        </CardHeader>
        <CardContent className="space-y-2">
          {plugin.description ? (
            <CardDescription className="line-clamp-3 leading-relaxed">
              {plugin.description}
            </CardDescription>
          ) : null}
          <div className="flex flex-wrap items-center gap-3 pt-1 text-[11px] text-muted-foreground">
            <span>
              {plugin.inputs.length}{" "}
              {plugin.inputs.length === 1 ? "input" : "inputs"}
            </span>
            <span aria-hidden>·</span>
            <span>
              {plugin.examples?.length ?? 0}{" "}
              {plugin.examples?.length === 1 ? "example" : "examples"}
            </span>
          </div>
        </CardContent>
      </Card>
    </button>
  );
}

function PluginSheet({
  plugin,
  open,
  onOpenChange,
}: {
  plugin: PluginSummary | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  if (!plugin) {
    return (
      <Sheet open={open} onOpenChange={onOpenChange}>
        <SheetContent side="right" />
      </Sheet>
    );
  }

  const Icon = iconFor(plugin.name);
  const tone = plugin.category ? CATEGORY_TONE[plugin.category] : null;
  const required = plugin.inputs.filter((i) => i.required);
  const optional = plugin.inputs.filter((i) => !i.required);

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        // sm:max-w-sm is the shadcn default; override so the
        // sheet takes roughly half the viewport on desktop, the
        // operator explicitly asked for that ratio.
        className="w-full sm:max-w-xl lg:max-w-2xl overflow-y-auto"
      >
        <SheetHeader className="space-y-3">
          <div className="flex items-center gap-3">
            <div
              className={cn(
                "flex size-10 shrink-0 items-center justify-center rounded-md border",
                tone ?? "border-border bg-muted/50",
              )}
              aria-hidden
            >
              <Icon className="size-5" />
            </div>
            <div className="min-w-0 flex-1">
              <SheetTitle className="font-mono">
                gocdnext/{plugin.name}
              </SheetTitle>
              {plugin.category ? (
                <Badge
                  variant="outline"
                  className={cn("mt-1 font-normal capitalize", tone)}
                >
                  {plugin.category}
                </Badge>
              ) : null}
            </div>
          </div>
          {plugin.description ? (
            <SheetDescription className="leading-relaxed">
              {plugin.description}
            </SheetDescription>
          ) : null}
        </SheetHeader>

        <div className="space-y-6 px-4 pb-6">
          {plugin.examples && plugin.examples.length > 0 ? (
            <section className="space-y-3">
              <h3 className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                <ClipboardList className="size-3.5" />
                Examples
              </h3>
              <div className="space-y-3">
                {plugin.examples.map((ex, i) => (
                  <ExampleBlock key={i} example={ex} />
                ))}
              </div>
            </section>
          ) : null}

          {required.length > 0 ? (
            <InputsSection label="Required" inputs={required} tone="required" />
          ) : null}
          {optional.length > 0 ? (
            <InputsSection label="Optional" inputs={optional} tone="optional" />
          ) : null}
          {plugin.inputs.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No inputs declared — the plugin reads everything from the
              workspace and its own image defaults.
            </p>
          ) : null}

          <SecretsHint plugin={plugin} />
        </div>
      </SheetContent>
    </Sheet>
  );
}

function ExampleBlock({ example }: { example: PluginExample }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(example.yaml);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // Ignore — older browsers / http contexts. The code is
      // readable on-screen either way.
    }
  }
  return (
    <div className="rounded-md border border-border bg-muted/30">
      <div className="flex items-start justify-between gap-2 px-3 pt-2">
        <div>
          {example.name ? (
            <p className="text-xs font-semibold">{example.name}</p>
          ) : null}
          {example.description ? (
            <p className="mt-0.5 text-xs text-muted-foreground">
              {example.description}
            </p>
          ) : null}
        </div>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={copy}
          className="h-7 shrink-0 text-xs"
        >
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <pre className="overflow-x-auto border-t border-border/60 px-3 py-2 text-xs leading-relaxed">
        <code className="font-mono">{example.yaml}</code>
      </pre>
    </div>
  );
}

function InputsSection({
  label,
  inputs,
  tone,
}: {
  label: string;
  inputs: PluginSummary["inputs"];
  tone: "required" | "optional";
}) {
  return (
    <section className="space-y-2">
      <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
        {label} ({inputs.length})
      </h3>
      <div className="divide-y divide-border overflow-hidden rounded-md border border-border">
        {inputs.map((i) => (
          <div key={i.name} className="space-y-1 px-3 py-2.5">
            <div className="flex flex-wrap items-center gap-2">
              <code className="font-mono text-sm font-semibold">{i.name}</code>
              {tone === "required" ? (
                <span className="inline-flex items-center rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-amber-700 dark:text-amber-400">
                  required
                </span>
              ) : (
                <span className="inline-flex items-center rounded-full border border-border bg-muted/40 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                  optional
                </span>
              )}
              {i.default ? (
                <span className="text-xs text-muted-foreground">
                  default{" "}
                  <code className="rounded bg-muted px-1 py-0.5 font-mono">
                    {i.default}
                  </code>
                </span>
              ) : null}
            </div>
            {i.description ? (
              <p className="text-sm leading-relaxed text-muted-foreground">
                {i.description}
              </p>
            ) : null}
          </div>
        ))}
      </div>
    </section>
  );
}

// SecretsHint surfaces the `secrets:` pattern at the bottom of
// every plugin sheet so operators see the wire-up even when the
// plugin doesn't take a credential-flavoured input. Keyed off
// the presence of inputs whose name hints at auth — best-effort
// but covers the common shapes (password/token/webhook/kubeconfig).
function SecretsHint({ plugin }: { plugin: PluginSummary }) {
  const hints = plugin.inputs.filter((i) =>
    /password|token|webhook|kubeconfig|secret/i.test(i.name),
  );
  if (hints.length === 0) return null;
  return (
    <section className="space-y-2 rounded-md border border-dashed border-border bg-muted/20 p-3">
      <h3 className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
        <KeyRound className="size-3.5" />
        Secrets pattern
      </h3>
      <p className="text-xs text-muted-foreground">
        Inputs{" "}
        {hints.map((h, i) => (
          <span key={h.name}>
            <code className="rounded bg-muted px-1 font-mono">{h.name}</code>
            {i < hints.length - 1 ? ", " : ""}
          </span>
        ))}{" "}
        accept <code className="rounded bg-muted px-1 font-mono">
          ${"{"}{"{"} VAR {"}"}{"}"}
        </code>{" "}
        interpolation. Declare the secret names under the job&apos;s{" "}
        <code className="rounded bg-muted px-1 font-mono">secrets:</code>{" "}
        list so the agent injects plaintext at dispatch and masks
        the value in every log line.
      </p>
    </section>
  );
}

function EmptyResult({ onClear }: { onClear: () => void }) {
  return (
    <div className="rounded-md border border-dashed border-border p-10 text-center">
      <Package
        className="mx-auto size-6 text-muted-foreground"
        aria-hidden
      />
      <p className="mt-3 text-sm font-medium">No plugins match</p>
      <p className="mt-1 text-sm text-muted-foreground">
        Try a different search or clear the filter.
      </p>
      <Button
        variant="outline"
        size="sm"
        className="mt-4"
        onClick={onClear}
      >
        <X className="mr-1 size-3.5" />
        Clear
      </Button>
    </div>
  );
}
