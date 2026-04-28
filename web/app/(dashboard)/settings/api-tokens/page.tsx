import type { Metadata } from "next";
import { KeyRound } from "lucide-react";

import { UserTokensManager } from "@/components/api-tokens/user-tokens-manager.client";
import { listMyAPITokens } from "@/server/queries/api-tokens";

export const metadata: Metadata = {
  title: "API tokens — gocdnext",
};

export const dynamic = "force-dynamic";

export default async function APITokensPage() {
  const tokens = await listMyAPITokens();
  return (
    <section className="space-y-6">
      <header className="space-y-1">
        <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <KeyRound className="size-6" aria-hidden /> API tokens
        </h1>
        <p className="text-sm text-muted-foreground">
          Personal tokens for the gocdnext CLI and external automation.
          Tokens inherit your role; revoke any you no longer need.
        </p>
      </header>
      <UserTokensManager initial={tokens} />
    </section>
  );
}
