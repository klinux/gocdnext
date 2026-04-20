import type { Metadata } from "next";

import { ChangePasswordForm } from "@/components/account/change-password-form.client";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { resolveAuthState } from "@/server/queries/auth";

export const metadata: Metadata = {
  title: "Account — gocdnext",
};

export const dynamic = "force-dynamic";

export default async function AccountPage() {
  const auth = await resolveAuthState();
  const isLocal =
    auth.mode === "authenticated" && auth.user.provider === "local";

  return (
    <div className="space-y-6 px-4 py-6 md:px-8">
      <header className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">Account</h1>
        <p className="text-sm text-muted-foreground">
          Your profile + credentials. OIDC users manage their profile on
          the identity provider.
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
    </div>
  );
}
