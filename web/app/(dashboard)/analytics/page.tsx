import type { Metadata } from "next";
import { BarChart3 } from "lucide-react";

import { DoraDashboard } from "@/components/analytics/dora-dashboard.client";
import { getDoraRollup, listLabelKeys } from "@/server/queries/analytics";

export const metadata: Metadata = {
  title: "Analytics — gocdnext",
};

export const dynamic = "force-dynamic";

type Search = { key?: string; window?: string };

function clampWindow(raw: string | undefined): number {
  const n = Number(raw);
  if (!Number.isFinite(n) || n < 1 || n > 365) return 30;
  return Math.floor(n);
}

export default async function AnalyticsPage({
  searchParams,
}: {
  searchParams: Promise<Search>;
}) {
  const sp = await searchParams;
  const keys = await listLabelKeys();
  const windowDays = clampWindow(sp.window);
  // Group by the requested key when it exists, else the first available key.
  const activeKey = sp.key && keys.includes(sp.key) ? sp.key : (keys[0] ?? "");
  const rollup = activeKey ? await getDoraRollup(activeKey, windowDays) : null;

  return (
    <section className="space-y-6">
      <div>
        <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <BarChart3 className="size-6" aria-hidden />
          Analytics
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          DORA metrics rolled up across projects, grouped by a project label
          (team, tier, domain). Deployment frequency, lead time, change-failure
          rate and time-to-restore — the org-level health view.
        </p>
      </div>

      {keys.length === 0 ? (
        <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
          No project labels yet. Add <span className="font-mono">key:value</span>{" "}
          labels (e.g. <span className="font-mono">team:payments</span>) in a
          project&apos;s Settings to group analytics by them.
        </p>
      ) : (
        <DoraDashboard
          keys={keys}
          activeKey={activeKey}
          windowDays={windowDays}
          groups={rollup?.groups ?? []}
        />
      )}
    </section>
  );
}
