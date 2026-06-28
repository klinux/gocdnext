"use client";

import type { Route } from "next";
import { useRouter } from "next/navigation";

import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

const WINDOWS = [
  { value: "7", label: "7 dias" },
  { value: "30", label: "30 dias" },
  { value: "90", label: "90 dias" },
];

// DoraToolbar drives the page query: group-by label key + trailing window. Both
// push to the URL (searchParams) so the RSC re-fetches. The "vs. N dias
// anteriores" caption mirrors the prior-window comparison the deltas use.
export function DoraToolbar({
  keys,
  activeKey,
  windowDays,
}: {
  keys: string[];
  activeKey: string;
  windowDays: number;
}) {
  const router = useRouter();
  const go = (key: string, win: number) =>
    router.push(`/analytics?key=${encodeURIComponent(key)}&window=${win}` as Route);

  return (
    <div className="flex flex-wrap items-center gap-3">
      <Field label="Agrupar por">
        <Select
          items={Object.fromEntries(keys.map((k) => [k, k]))}
          value={activeKey}
          onValueChange={(v) => v && go(v, windowDays)}
        >
          <SelectTrigger aria-label="Agrupar por" className="h-9 w-40 font-mono">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {keys.map((k) => (
              <SelectItem key={k} value={k}>
                {k}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>

      <Field label="Janela">
        <Select
          items={Object.fromEntries(WINDOWS.map((w) => [w.value, w.label]))}
          value={String(windowDays)}
          onValueChange={(v) => v && go(activeKey, Number(v))}
        >
          <SelectTrigger aria-label="Janela" className="h-9 w-32">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {WINDOWS.map((w) => (
              <SelectItem key={w.value} value={w.value}>
                {w.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>

      <span className="ml-auto text-xs text-muted-foreground">
        vs. {windowDays} dias anteriores
      </span>
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center gap-2">
      <span className="text-xs text-muted-foreground">{label}</span>
      {children}
    </div>
  );
}
