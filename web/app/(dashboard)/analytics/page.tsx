import type { Metadata } from "next";
import type { ReactNode } from "react";
import { LineChart } from "lucide-react";

import { DoraBenchmark } from "@/components/analytics/dora-benchmark";
import { DoraBottleneck } from "@/components/analytics/dora-bottleneck";
import { DoraDeployFrequency } from "@/components/analytics/dora-deploy-frequency";
import { DoraHeroCards, TierChip } from "@/components/analytics/dora-hero-cards";
import { DoraLeaderboard } from "@/components/analytics/dora-leaderboard.client";
import { orgTier } from "@/components/analytics/dora-metrics";
import { DoraCompliance } from "@/components/analytics/dora-compliance";
import { DoraMovers } from "@/components/analytics/dora-movers.client";
import { DoraReliability } from "@/components/analytics/dora-reliability";
import { DoraToolbar } from "@/components/analytics/dora-toolbar.client";
import { TIER_LABEL } from "@/lib/dora";
import {
  getComplianceCoverage,
  getDoraOverview,
  getReliability,
  listEnvironments,
  listLabelKeys,
} from "@/server/queries/analytics";

export const metadata: Metadata = {
  title: "Analytics — gocdnext",
};

export const dynamic = "force-dynamic";

type Search = { key?: string; window?: string; env?: string };

function clampWindow(raw: string | undefined): number {
  const n = Number(raw);
  if (!Number.isFinite(n) || n < 1 || n > 365) return 30;
  return Math.floor(n);
}

function SectionLabel({ children }: { children: ReactNode }) {
  return (
    <div className="flex items-center gap-2.5 font-mono text-[11px] font-semibold uppercase tracking-wide text-muted-foreground/70">
      {children}
      <span className="h-px flex-1 bg-border" />
    </div>
  );
}

export default async function AnalyticsPage({
  searchParams,
}: {
  searchParams: Promise<Search>;
}) {
  const sp = await searchParams;
  const keys = await listLabelKeys();

  return (
    <div className="space-y-6">
      <header className="space-y-1.5">
        <h1 className="flex items-center gap-2 text-2xl font-bold tracking-tight">
          <LineChart className="size-6 text-brand-500" aria-hidden />
          Analytics
        </h1>
        <p className="max-w-[880px] text-sm text-muted-foreground">
          The four DORA metrics consolidated across all projects, classified by
          performance tier. Where teams ship fast, where they break, and where
          time is lost — engineering health at the org level.
        </p>
      </header>

      {keys.length === 0 ? <EmptyState /> : <Dashboard sp={sp} keys={keys} />}
    </div>
  );
}

async function Dashboard({ sp, keys }: { sp: Search; keys: string[] }) {
  const windowDays = clampWindow(sp.window);
  const activeKey = sp.key && keys.includes(sp.key) ? sp.key : (keys[0] ?? "");
  const environments = await listEnvironments(activeKey);
  const activeEnv = sp.env && environments.includes(sp.env) ? sp.env : "";
  const [ov, reliability, compliance] = await Promise.all([
    getDoraOverview(activeKey, windowDays, activeEnv),
    getReliability(activeKey, windowDays),
    getComplianceCoverage(activeKey),
  ]);
  const tier = orgTier(ov);

  return (
    <>
      <DoraToolbar
        keys={keys}
        activeKey={activeKey}
        windowDays={windowDays}
        environments={environments}
        activeEnv={activeEnv}
      />

      <div className="space-y-3.5">
        <SectionLabel>
          Organization summary
          <TierChip
            tier={tier}
            label={`${TIER_LABEL[tier]} performer`}
            className="text-[11px]"
          />
          <span className="font-normal normal-case tracking-normal text-muted-foreground/70">
            {ov.teams.length} groups · {ov.current.deploys_total} deploys in
            window
          </span>
        </SectionLabel>
        <DoraHeroCards overview={ov} />
      </div>

      <div className="space-y-3.5">
        <SectionLabel>Trend &amp; bottleneck</SectionLabel>
        <div className="grid gap-3.5 lg:grid-cols-2">
          <DoraDeployFrequency
            daily={ov.daily}
            windowDays={windowDays}
            freqPerDay={ov.current.deploy_freq_per_day}
          />
          <DoraBottleneck bottleneck={ov.bottleneck} />
        </div>
      </div>

      <div className="space-y-3.5">
        <SectionLabel>
          Performance by <span className="font-mono normal-case">{activeKey}</span>
        </SectionLabel>
        {ov.teams.length === 0 ? (
          <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
            No deploys in this window for any{" "}
            <span className="font-mono">{activeKey}</span> group. DORA metrics
            derive from deploy markers — projects must run a job with a{" "}
            <span className="font-mono">deploy:</span> block.
          </p>
        ) : (
          <DoraLeaderboard teams={ov.teams} groupKey={activeKey} />
        )}
      </div>

      <div className="space-y-3.5">
        <SectionLabel>Throughput &amp; reliability</SectionLabel>
        <DoraReliability
          report={reliability}
          groupKey={activeKey}
          envFiltered={activeEnv !== ""}
        />
      </div>

      <div className="space-y-3.5">
        <SectionLabel>Compliance posture</SectionLabel>
        <DoraCompliance report={compliance} groupKey={activeKey} />
      </div>

      {ov.teams.length > 0 ? (
        <div className="space-y-3.5">
          <SectionLabel>Highlights</SectionLabel>
          <DoraMovers overview={ov} windowDays={windowDays} />
        </div>
      ) : null}

      <DoraBenchmark />
    </>
  );
}

function EmptyState() {
  return (
    <p className="rounded-md border border-dashed p-8 text-center text-sm text-muted-foreground">
      No project labels yet. DORA metrics are grouped by a{" "}
      <span className="font-mono">key:value</span> label (e.g.{" "}
      <span className="font-mono">team:payments</span>) — add labels in{" "}
      <span className="font-mono">Project → Settings</span> to enable grouping.
    </p>
  );
}
