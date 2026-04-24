"use client";

import type { ReactNode } from "react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { StatusPill } from "@/components/shared/status-pill";
import { WebhookEndpointRow } from "@/components/settings/webhook-endpoint-row.client";
import { cn } from "@/lib/utils";

type Tone = "ready" | "partial" | "off";

type Props = {
  provider: "github" | "gitlab" | "bitbucket";
  tone: Tone;
  statusLabel: string;
  headline: string;
  description: ReactNode;
  webhookEndpoint: string;
  authSummary: ReactNode;
  children?: ReactNode;
};

const toneClasses: Record<Tone, string> = {
  ready: "text-emerald-600 dark:text-emerald-400",
  partial: "text-amber-600 dark:text-amber-400",
  off: "text-muted-foreground",
};

// ProviderCard is the symmetric tile each SCM provider uses on
// /settings/integrations. Same shape across GitHub / GitLab /
// Bitbucket so the page reads as a 3-up grid of first-class
// peers, not GitHub + appendix.
export function ProviderCard({
  provider,
  tone,
  statusLabel,
  headline,
  description,
  webhookEndpoint,
  authSummary,
  children,
}: Props) {
  return (
    <Card className="flex flex-col">
      <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <ProviderLogo provider={provider} className={cn("size-5", toneClasses[tone])} />
            <CardTitle className="text-base">{headline}</CardTitle>
          </div>
          <CardDescription className="mt-1.5">{description}</CardDescription>
        </div>
        <StatusPill tone={tone === "ready" ? "success" : tone === "partial" ? "warning" : "neutral"}>
          {statusLabel}
        </StatusPill>
      </CardHeader>
      <CardContent className="flex flex-1 flex-col gap-4">
        <div>
          <p className="mb-1.5 text-xs font-medium text-muted-foreground">
            Webhook endpoint
          </p>
          <WebhookEndpointRow endpoint={webhookEndpoint} />
        </div>
        <div className="rounded-md border bg-muted/30 p-3 text-xs">
          <p className="font-medium">Authentication</p>
          <div className="mt-1 text-muted-foreground">{authSummary}</div>
        </div>
        {children}
      </CardContent>
    </Card>
  );
}

function ProviderLogo({
  provider,
  className,
}: {
  provider: "github" | "gitlab" | "bitbucket";
  className?: string;
}) {
  // Inline SVGs keep the bundle lean and render deterministically
  // across themes — no extra icon lib.
  switch (provider) {
    case "github":
      return (
        <svg
          className={className}
          viewBox="0 0 24 24"
          fill="currentColor"
          aria-hidden
        >
          <path d="M12 .5C5.37.5 0 5.87 0 12.5c0 5.3 3.44 9.8 8.21 11.4.6.11.82-.26.82-.58 0-.28-.01-1.03-.02-2.03-3.34.73-4.04-1.61-4.04-1.61-.55-1.4-1.34-1.77-1.34-1.77-1.09-.75.08-.74.08-.74 1.21.09 1.85 1.24 1.85 1.24 1.07 1.84 2.81 1.31 3.5 1 .11-.78.42-1.31.76-1.61-2.67-.31-5.47-1.34-5.47-5.95 0-1.31.47-2.38 1.23-3.22-.12-.3-.53-1.52.12-3.17 0 0 1.01-.32 3.31 1.23.96-.27 1.98-.4 3-.4s2.04.13 3 .4c2.3-1.55 3.31-1.23 3.31-1.23.65 1.65.24 2.87.12 3.17.77.84 1.23 1.91 1.23 3.22 0 4.62-2.81 5.63-5.48 5.93.43.37.81 1.1.81 2.22 0 1.6-.02 2.89-.02 3.28 0 .32.22.7.83.58A12.01 12.01 0 0024 12.5C24 5.87 18.63.5 12 .5z" />
        </svg>
      );
    case "gitlab":
      return (
        <svg
          className={className}
          viewBox="0 0 24 24"
          fill="currentColor"
          aria-hidden
        >
          <path d="M22.75 9.31L21.64 5.85a.52.52 0 00-1 0l-1.39 4.28H4.74L3.35 5.85a.52.52 0 00-1 0L1.25 9.31a1 1 0 00.36 1.12L12 18.17l10.39-7.74a1 1 0 00.36-1.12z" />
        </svg>
      );
    case "bitbucket":
      return (
        <svg
          className={className}
          viewBox="0 0 24 24"
          fill="currentColor"
          aria-hidden
        >
          <path d="M.778 1.213a.768.768 0 00-.768.892l3.263 19.81c.084.5.515.868 1.022.874H19.77a.768.768 0 00.77-.644l3.27-20.04a.768.768 0 00-.768-.892zM14.52 15.53H9.522L8.17 8.466h7.561z" />
        </svg>
      );
  }
}
