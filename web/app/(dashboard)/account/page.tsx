import type { Metadata } from "next";
import { UserCog } from "lucide-react";

import { ChangePasswordForm } from "@/components/account/change-password-form.client";
import { UserTokensManager } from "@/components/api-tokens/user-tokens-manager.client";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { listMyAPITokens } from "@/server/queries/api-tokens";
import { resolveAuthState } from "@/server/queries/auth";

export const metadata: Metadata = {
  title: "Account — gocdnext",
};

export const dynamic = "force-dynamic";

export default async function AccountPage() {
  const [auth, tokens] = await Promise.all([
    resolveAuthState(),
    listMyAPITokens(),
  ]);
  const isLocal =
    auth.mode === "authenticated" && auth.user.provider === "local";

  return (
    <section className="space-y-6">
      <header className="space-y-1">
        <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <UserCog className="size-6" aria-hidden /> Account
        </h1>
        <p className="text-sm text-muted-foreground">
          Your profile, credentials, and personal API tokens. OIDC users
          manage their profile on the identity provider.
        </p>
      </header>

      {auth.mode === "authenticated" ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">Signed in as</CardTitle>
            <CardDescription>
              {auth.user.name || auth.user.email}{" "}
              <span className="text-muted-foreground">
                ({auth.user.email}) · {auth.user.role} · via{" "}
                <code className="font-mono">{auth.user.provider}</code>
              </span>
            </CardDescription>
          </CardHeader>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Change password</CardTitle>
          <CardDescription>
            {isLocal
              ? "Rotate your local-account password. The old session stays valid after a change."
              : "Not available for your account — password changes happen at the identity provider."}
          </CardDescription>
        </CardHeader>
        {isLocal ? (
          <CardContent>
            <ChangePasswordForm />
          </CardContent>
        ) : null}
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">API tokens</CardTitle>
          <CardDescription>
            Personal tokens for the gocdnext CLI and external automation.
            Tokens inherit your role; revoke any you no longer need.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <UserTokensManager initial={tokens} />
        </CardContent>
      </Card>
    </section>
  );
}
