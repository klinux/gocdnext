import type { Metadata } from "next";
import { Check, X } from "lucide-react";

import { VCSIntegrationsAdminView } from "@/components/settings/vcs-integrations.client";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { StatusPill } from "@/components/shared/status-pill";
import {
  getGitHubIntegration,
  listVCSIntegrations,
} from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — Integrations",
};

export const dynamic = "force-dynamic";

export default async function IntegrationsPage() {
  // Fetch both the summary booleans (env-derived wiring check) and
  // the full VCS integration list (DB + registry view). Runs in
  // parallel so the page load stays snappy.
  const [gh, vcs] = await Promise.all([
    getGitHubIntegration(),
    listVCSIntegrations(),
  ]);

  const rows: { label: string; value: boolean; hint?: string }[] = [
    {
      label: "GitHub App",
      value: gh.github_app_configured,
      hint: "An App client is active (from env or DB).",
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
      hint: "Disabled pending multi-scm_source refactor.",
    },
  ];

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Wiring summary</CardTitle>
          <CardDescription>
            Quick overview of which control-plane features are reachable right
            now. Configure integrations below — the flags refresh on save.
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
                  <StatusPill tone="success" icon={Check}>configured</StatusPill>
                ) : (
                  <StatusPill tone="neutral" icon={X}>off</StatusPill>
                )}
              </li>
            ))}
          </ul>
        </CardContent>
      </Card>

      <VCSIntegrationsAdminView
        integrations={vcs.integrations}
        active={vcs.active}
      />

      <Card>
        <CardHeader>
          <CardTitle className="text-base">GitLab · Bitbucket</CardTitle>
          <CardDescription>
            Not yet supported. Planned alongside the auth OIDC work: once the
            registry is polyglot, the webhook vocabulary follows.
          </CardDescription>
        </CardHeader>
      </Card>
    </div>
  );
}
