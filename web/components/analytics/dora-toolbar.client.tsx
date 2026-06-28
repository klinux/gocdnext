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
  { value: "7", label: "7 days" },
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
];

// "All environments" sentinel — Base UI Select needs a non-empty value, so we
// map this to "" (no filter) when building the URL.
const ALL_ENV = "all";

// DoraToolbar drives the page query: group-by label key, trailing window, and
// the deploy-environment filter. All push to the URL (searchParams) so the RSC
// re-fetches. The "vs. previous N days" caption mirrors the prior-window
// comparison the deltas use.
export function DoraToolbar({
  keys,
  activeKey,
  windowDays,
  environments,
  activeEnv,
}: {
  keys: string[];
  activeKey: string;
  windowDays: number;
  environments: string[];
  activeEnv: string;
}) {
  const router = useRouter();
  const go = (key: string, win: number, env: string) => {
    const envQ = env ? `&env=${encodeURIComponent(env)}` : "";
    router.push(`/analytics?key=${encodeURIComponent(key)}&window=${win}${envQ}` as Route);
  };

  const envItems: Record<string, string> = {
    [ALL_ENV]: "All environments",
    ...Object.fromEntries(environments.map((e) => [e, e])),
  };

  return (
    <div className="flex flex-wrap items-center gap-3">
      <Field label="Group by">
        <Select
          items={Object.fromEntries(keys.map((k) => [k, k]))}
          value={activeKey}
          onValueChange={(v) => v && go(v, windowDays, activeEnv)}
        >
          <SelectTrigger aria-label="Group by" className="h-9 w-40 font-mono">
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

      <Field label="Window">
        <Select
          items={Object.fromEntries(WINDOWS.map((w) => [w.value, w.label]))}
          value={String(windowDays)}
          onValueChange={(v) => v && go(activeKey, Number(v), activeEnv)}
        >
          <SelectTrigger aria-label="Window" className="h-9 w-32">
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

      {environments.length > 0 ? (
        <Field label="Environment">
          <Select
            items={envItems}
            value={activeEnv || ALL_ENV}
            onValueChange={(v) =>
              v && go(activeKey, windowDays, v === ALL_ENV ? "" : v)
            }
          >
            <SelectTrigger aria-label="Environment" className="h-9 w-44 font-mono">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {Object.entries(envItems).map(([value, label]) => (
                <SelectItem key={value} value={value}>
                  {label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
      ) : null}

      <span className="ml-auto text-xs text-muted-foreground">
        vs. previous {windowDays} days
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
