import type { Metadata } from "next";
import type { ReactNode } from "react";
import { LineChart } from "lucide-react";

import { DoraBenchmark } from "@/components/analytics/dora-benchmark";
import { DoraHeroCards, TierChip } from "@/components/analytics/dora-hero-cards";
import { DoraLeaderboard } from "@/components/analytics/dora-leaderboard.client";
import { orgTier } from "@/components/analytics/dora-metrics";
import { DoraToolbar } from "@/components/analytics/dora-toolbar.client";
import { TIER_LABEL } from "@/lib/dora";
import { getDoraOverview, listLabelKeys } from "@/server/queries/analytics";

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
          As quatro métricas DORA consolidadas entre todos os projetos, com
          classificação de desempenho por faixa. Onde o time entrega rápido,
          onde quebra, e onde o tempo se perde — a saúde de engenharia em nível
          de organização.
        </p>
      </header>

      {keys.length === 0 ? <EmptyState /> : <Dashboard sp={sp} keys={keys} />}
    </div>
  );
}

async function Dashboard({ sp, keys }: { sp: Search; keys: string[] }) {
  const windowDays = clampWindow(sp.window);
  const activeKey = sp.key && keys.includes(sp.key) ? sp.key : (keys[0] ?? "");
  const ov = await getDoraOverview(activeKey, windowDays);
  const tier = orgTier(ov);

  return (
    <>
      <DoraToolbar keys={keys} activeKey={activeKey} windowDays={windowDays} />

      <div className="space-y-3.5">
        <SectionLabel>
          Resumo da organização
          <TierChip
            tier={tier}
            label={`${TIER_LABEL[tier]} performer`}
            className="text-[11px]"
          />
          <span className="font-normal normal-case tracking-normal text-muted-foreground/70">
            {ov.teams.length} grupos · {ov.current.deploys_total} deploys na
            janela
          </span>
        </SectionLabel>
        <DoraHeroCards overview={ov} />
      </div>

      <div className="space-y-3.5">
        <SectionLabel>
          Desempenho por <span className="font-mono normal-case">{activeKey}</span>
        </SectionLabel>
        {ov.teams.length === 0 ? (
          <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
            Nenhum deploy nesta janela para grupos de{" "}
            <span className="font-mono">{activeKey}</span>. As métricas DORA
            derivam de marcadores de deploy — os projetos precisam rodar um job
            com bloco <span className="font-mono">deploy:</span>.
          </p>
        ) : (
          <DoraLeaderboard teams={ov.teams} groupKey={activeKey} />
        )}
      </div>

      <DoraBenchmark />
    </>
  );
}

function EmptyState() {
  return (
    <p className="rounded-md border border-dashed p-8 text-center text-sm text-muted-foreground">
      Nenhuma label de projeto ainda. As métricas DORA são agrupadas por uma
      label <span className="font-mono">key:value</span> (ex.{" "}
      <span className="font-mono">team:payments</span>) — adicione labels em{" "}
      <span className="font-mono">Projeto → Settings</span> para habilitar o
      agrupamento por time.
    </p>
  );
}
