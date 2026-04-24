import type { Metadata } from "next";
import { Check, X } from "lucide-react";

import { VCSIntegrationsAdminView } from "@/components/settings/vcs-integrations.client";
import { WebhookEndpointRow } from "@/components/settings/webhook-endpoint-row.client";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { StatusPill } from "@/components/shared/status-pill";
import {
  getIntegrationsSummary,
  listVCSIntegrations,
} from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — Integrations",
};

export const dynamic = "force-dynamic";

export default async function IntegrationsPage() {
  const [summary, vcs] = await Promise.all([
    getIntegrationsSummary(),
    listVCSIntegrations(),
  ]);

  const wiringRows: { label: string; value: boolean; hint?: string }[] = [
    {
      label: "Public base URL",
      value: summary.public_base_set,
      hint: "Required for auto-register across every provider.",
    },
    {
      label: "GitHub App",
      value: summary.github.app_configured,
      hint: "An App client is active (from env or DB).",
    },
    {
      label: "Checks reporter",
      value: summary.github.checks_reporter_on,
      hint: "Posts run status to GitHub Checks API.",
    },
    {
      label: "GitHub auto-register",
      value: summary.github.auto_register_on,
      hint: "Installs push webhook on bound GitHub repos.",
    },
    {
      label: "GitLab auto-register",
      value: summary.gitlab.auto_register_on,
      hint: "Ready to install hooks; per-project PAT lives on scm_source.",
    },
    {
      label: "Bitbucket auto-register",
      value: summary.bitbucket.auto_register_on,
      hint: "Ready to install hooks; per-project App Password lives on scm_source.",
    },
  ];

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Wiring summary</CardTitle>
          <CardDescription>
            Which control-plane features are reachable right now. Configure
            integrations below — the flags refresh on save.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <ul className="divide-y">
            {wiringRows.map((r) => (
              <li
                key={r.label}
                className="flex items-center justify-between py-3"
              >
                <div className="min-w-0">
                  <p className="text-sm font-medium">{r.label}</p>
                  {r.hint ? (
                    <p className="text-xs text-muted-foreground">{r.hint}</p>
                  ) : null}
                </div>
                {r.value ? (
                  <StatusPill tone="success" icon={Check}>
                    configured
                  </StatusPill>
                ) : (
                  <StatusPill tone="neutral" icon={X}>
                    off
                  </StatusPill>
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
          <CardTitle className="text-base">GitLab</CardTitle>
          <CardDescription>
            Bind a GitLab project to a gocdnext project, paste a Personal
            Access Token with <code className="rounded bg-muted px-1 text-xs">
              api
            </code>{" "}
            scope as the auth_ref, and gocdnext installs the push webhook for
            you. Self-hosted GitLab works the same way — the auto-register
            call follows the clone URL back to the host.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <div>
            <p className="mb-1 text-xs font-medium text-muted-foreground">
              Webhook endpoint
            </p>
            <WebhookEndpointRow endpoint={summary.gitlab.webhook_endpoint} />
          </div>
          <div className="flex items-start gap-2 rounded-md border border-border bg-muted/30 p-3 text-xs">
            <div>
              <p className="font-medium">Required PAT scope</p>
              <p className="mt-0.5 text-muted-foreground">
                <code className="rounded bg-muted px-1">api</code> (not{" "}
                <code className="rounded bg-muted px-1">read_api</code> —
                we need hook-write access).
              </p>
            </div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Bitbucket Cloud</CardTitle>
          <CardDescription>
            Paste either a Bitbucket OAuth access token or a{" "}
            <code className="rounded bg-muted px-1 text-xs">
              user:app_password
            </code>{" "}
            as the auth_ref. gocdnext installs an HMAC-signed push webhook at
            bind time.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <div>
            <p className="mb-1 text-xs font-medium text-muted-foreground">
              Webhook endpoint
            </p>
            <WebhookEndpointRow endpoint={summary.bitbucket.webhook_endpoint} />
          </div>
          <div className="flex items-start gap-2 rounded-md border border-border bg-muted/30 p-3 text-xs">
            <div>
              <p className="font-medium">Required App Password scope</p>
              <p className="mt-0.5 text-muted-foreground">
                <code className="rounded bg-muted px-1">webhooks</code>{" "}
                (read + write) plus{" "}
                <code className="rounded bg-muted px-1">
                  repositories:read
                </code>{" "}
                to fetch pipeline YAML.
              </p>
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
