import type { Metadata } from "next";
import { Bot } from "lucide-react";

import { ServiceAccountsManager } from "@/components/service-accounts/sa-manager.client";
import {
  listSATokens,
  listServiceAccounts,
} from "@/server/queries/api-tokens";

export const metadata: Metadata = {
  title: "Service accounts — gocdnext",
};

export const dynamic = "force-dynamic";

export default async function ServiceAccountsPage() {
  const accounts = await listServiceAccounts();
  // Pre-fetch tokens for every SA so the manager has them on first
  // paint. N+1 over a list of <50 SAs is fine; switch to a join
  // when this grows past that.
  const tokens = await Promise.all(accounts.map((sa) => listSATokens(sa.id)));
  const tokensBySA: Record<string, Awaited<ReturnType<typeof listSATokens>>> = {};
  accounts.forEach((sa, i) => {
    tokensBySA[sa.id] = tokens[i] ?? [];
  });

  return (
    <section className="space-y-6">
      <header className="space-y-1">
        <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <Bot className="size-6" aria-hidden /> Service accounts
        </h1>
        <p className="text-sm text-muted-foreground">
          Machine identities for CI orchestration, deploy bots, and any
          automation that talks to the gocdnext API. Each account has
          its own role + zero-or-more tokens.
        </p>
      </header>
      <ServiceAccountsManager initial={accounts} tokensBySA={tokensBySA} />
    </section>
  );
}
