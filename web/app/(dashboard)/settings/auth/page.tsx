import type { Metadata } from "next";

import { AuthProvidersAdminView } from "@/components/settings/auth-providers.client";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { listConfiguredAuthProviders } from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — Auth",
};

export const dynamic = "force-dynamic";

export default async function AuthSettingsPage() {
  let payload;
  try {
    payload = await listConfiguredAuthProviders();
  } catch (err) {
    return (
      <Card className="border-destructive/50">
        <CardHeader>
          <CardTitle>Failed to load auth providers</CardTitle>
          <CardDescription>
            {err instanceof Error ? err.message : String(err)}
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }

  return (
    <AuthProvidersAdminView
      enabled={payload.enabled}
      providers={payload.providers}
      envOnly={payload.env_only ?? []}
    />
  );
}
