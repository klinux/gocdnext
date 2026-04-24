import type { Metadata } from "next";
import { AlertCircle } from "lucide-react";

import { ProviderCard } from "@/components/settings/provider-card.client";
import { SCMCredentialManager } from "@/components/settings/scm-credential-manager.client";
import { VCSIntegrationsAdminView } from "@/components/settings/vcs-integrations.client";
import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  getIntegrationsSummary,
  listSCMCredentials,
  listVCSIntegrations,
} from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — Integrations",
};

export const dynamic = "force-dynamic";

export default async function IntegrationsPage() {
  const [summary, vcs, creds] = await Promise.all([
    getIntegrationsSummary(),
    listVCSIntegrations(),
    listSCMCredentials().catch(() => ({ credentials: [] })),
  ]);

  const gitlabCreds = creds.credentials.filter((c) => c.provider === "gitlab");
  const bitbucketCreds = creds.credentials.filter(
    (c) => c.provider === "bitbucket",
  );

  const githubTone: "ready" | "partial" | "off" =
    summary.github.auto_register_on
      ? "ready"
      : summary.github.app_configured
        ? "partial"
        : "off";

  const gitlabTone: "ready" | "partial" | "off" =
    summary.public_base_set ? "ready" : "off";

  const bitbucketTone: "ready" | "partial" | "off" =
    summary.public_base_set ? "ready" : "off";

  return (
    <div className="space-y-6">
      {!summary.public_base_set ? (
        <Card className="border-amber-500/40 bg-amber-500/5">
          <CardHeader className="flex-row items-start gap-3 space-y-0">
            <AlertCircle className="mt-0.5 size-5 shrink-0 text-amber-500" />
            <div>
              <CardTitle className="text-base">
                Public base URL not configured
              </CardTitle>
              <CardDescription className="mt-1">
                Set{" "}
                <code className="rounded bg-muted px-1 text-xs">
                  GOCDNEXT_PUBLIC_BASE
                </code>{" "}
                to the URL where this server is reachable from the internet
                (or from your SCM provider) so auto-register can install push
                webhooks. Without it, every provider card below stays in
                read-only mode.
              </CardDescription>
            </div>
          </CardHeader>
        </Card>
      ) : null}

      <div>
        <h3 className="text-sm font-medium text-muted-foreground">
          SCM providers
        </h3>
        <p className="mt-0.5 text-xs text-muted-foreground">
          Bind a repo on a project and gocdnext installs the webhook for you
          — no paste, no script.
        </p>
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <ProviderCard
          provider="github"
          tone={githubTone}
          statusLabel={
            githubTone === "ready"
              ? "auto-register on"
              : githubTone === "partial"
                ? "app configured"
                : "app not configured"
          }
          headline="GitHub"
          description="App-based integration. Install the gocdnext App on your org or repos; no per-project token required."
          webhookEndpoint={summary.github.webhook_endpoint}
          authSummary={
            <>
              Uses an installed GitHub App. Configure the app credentials
              below. Per-project <code>auth_ref</code> is only needed for
              fine-grained PAT fallback.
            </>
          }
        />

        <ProviderCard
          provider="gitlab"
          tone={gitlabTone}
          statusLabel={gitlabTone === "ready" ? "ready" : "needs public base"}
          headline="GitLab"
          description="PAT-based. Works with gitlab.com and self-hosted — the clone URL determines the API host."
          webhookEndpoint={summary.gitlab.webhook_endpoint}
          authSummary={
            <>
              Per-project or org-level Personal Access Token with scope{" "}
              <code className="rounded bg-muted px-1">api</code>. Org-level
              credentials below cover every project whose clone URL matches
              the host.
            </>
          }
        >
          <SCMCredentialManager
            provider="gitlab"
            credentials={gitlabCreds}
            defaultHost="gitlab.com"
            apiBasePlaceholder="https://gitlab.internal/api/v4"
            authHint="Personal Access Token"
          />
        </ProviderCard>

        <ProviderCard
          provider="bitbucket"
          tone={bitbucketTone}
          statusLabel={bitbucketTone === "ready" ? "ready" : "needs public base"}
          headline="Bitbucket Cloud"
          description="OAuth token or user:app_password. Bitbucket Server (self-hosted) not supported yet."
          webhookEndpoint={summary.bitbucket.webhook_endpoint}
          authSummary={
            <>
              OAuth access token or{" "}
              <code className="rounded bg-muted px-1">user:app_password</code>.
              Required App Password scopes:{" "}
              <code className="rounded bg-muted px-1">webhooks</code> +{" "}
              <code className="rounded bg-muted px-1">repositories:read</code>.
            </>
          }
        >
          <SCMCredentialManager
            provider="bitbucket"
            credentials={bitbucketCreds}
            defaultHost="bitbucket.org"
            apiBasePlaceholder="(leave empty for bitbucket.org)"
            authHint="OAuth token or user:app_password"
          />
        </ProviderCard>
      </div>

      <div>
        <h3 className="text-sm font-medium text-muted-foreground">
          GitHub App credentials
        </h3>
        <p className="mt-0.5 text-xs text-muted-foreground">
          Manage installed Apps below. Only GitHub uses this tab — GitLab /
          Bitbucket auth lives per-project on the scm_source binding.
        </p>
      </div>
      <VCSIntegrationsAdminView
        integrations={vcs.integrations}
        active={vcs.active}
      />
    </div>
  );
}

