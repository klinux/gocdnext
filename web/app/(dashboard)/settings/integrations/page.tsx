import type { Metadata } from "next";
import { Check, X } from "lucide-react";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { getGitHubIntegration } from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — Integrations",
};

export default async function IntegrationsPage() {
  const gh = await getGitHubIntegration();

  const rows: { label: string; value: boolean; hint?: string }[] = [
    {
      label: "GitHub App",
      value: gh.github_app_configured,
      hint: "APP_ID + private key env vars are set.",
    },
    {
      label: "Webhook token",
      value: gh.webhook_token_set,
      hint: "HMAC secret signs incoming push/PR events.",
    },
    {
      label: "Public base URL",
      value: gh.public_base_set,
      hint: "Needed for auto-register and Checks callbacks.",
    },
    {
      label: "Checks reporter",
      value: gh.checks_reporter_on,
      hint: "Posts run status to GitHub Checks API.",
    },
    {
      label: "Auto-register",
      value: gh.auto_register_on,
      hint: "Installs webhooks on new repos at Apply time.",
    },
  ];

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>GitHub</CardTitle>
          <CardDescription>
            Values are surfaced as booleans only. The concrete secrets and URLs
            live in <code className="font-mono">GOCDNEXT_GITHUB_*</code> env
            vars at the control-plane boot.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <ul className="divide-y">
            {rows.map((r) => (
              <li key={r.label} className="flex items-center justify-between py-3">
                <div className="min-w-0">
                  <p className="text-sm font-medium">{r.label}</p>
                  {r.hint ? (
                    <p className="text-xs text-muted-foreground">{r.hint}</p>
                  ) : null}
                </div>
                {r.value ? (
                  <span className="inline-flex items-center gap-1 rounded-md bg-status-success-bg px-2 py-0.5 text-xs font-medium text-status-success-fg">
                    <Check className="size-3.5" /> configured
                  </span>
                ) : (
                  <span className="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
                    <X className="size-3.5" /> off
                  </span>
                )}
              </li>
            ))}
          </ul>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>GitLab · Bitbucket</CardTitle>
          <CardDescription>
            Not yet supported. Planned alongside the multi-provider auth work
            (UI.6): once OIDC is generic, the webhook vocabulary can follow.
          </CardDescription>
        </CardHeader>
      </Card>
    </div>
  );
}
