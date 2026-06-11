import type { Metadata } from "next";
import { ExternalLink, Globe } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { OIDCKeysManager } from "@/components/settings/oidc-keys.client";
import { listOIDCKeys } from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — OIDC issuer",
};

// force-dynamic: rotation changes the key list out-of-band (API,
// another admin, boot of a fresh replica) and this page is the
// operator's source of truth during an incident.
export const dynamic = "force-dynamic";

// Relative on purpose: the discovery endpoints live on the server's
// PUBLIC base URL, which the web pod doesn't know — but the BROWSER
// is already on the public host in the standard single-ingress
// deployment, so a relative href lands on the right origin.
const wellKnown = [
  {
    label: "openid-configuration",
    path: "/.well-known/openid-configuration",
  },
  { label: "jwks.json", path: "/.well-known/jwks.json" },
];

export default async function OIDCSettingsPage() {
  const { keys } = await listOIDCKeys();

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <div className="flex items-center gap-2">
            <Globe className="size-4 text-muted-foreground" aria-hidden />
            <CardTitle className="text-base">Discovery endpoints</CardTitle>
          </div>
          <CardDescription>
            Point cloud / Vault trust configuration at these. Only the
            federation target&apos;s verifier needs to reach them — see the{" "}
            <a
              href="https://klinux.github.io/gocdnext/docs/concepts/id-tokens/"
              target="_blank"
              rel="noreferrer"
              className="underline underline-offset-2"
            >
              id_tokens concept page
            </a>{" "}
            for per-provider reachability notes.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-2">
          {wellKnown.map((e) => (
            <a
              key={e.path}
              href={e.path}
              target="_blank"
              rel="noreferrer"
              className="flex items-center justify-between gap-3 rounded-md border bg-muted/20 px-3 py-2 text-sm hover:bg-muted/40"
            >
              <code className="font-mono text-xs">{e.path}</code>
              <ExternalLink
                className="size-3.5 shrink-0 text-muted-foreground"
                aria-hidden
              />
            </a>
          ))}
        </CardContent>
      </Card>

      <OIDCKeysManager keys={keys} />
    </div>
  );
}
