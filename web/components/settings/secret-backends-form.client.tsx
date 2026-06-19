"use client";

import { useMemo } from "react";
import { Info } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { secretBackendSources } from "@/lib/validations";
import type { SecretBackend, SecretBackendSource } from "@/types/api";

import { SecretBackendPanel } from "./secret-backend-panel.client";

type Props = { initial: SecretBackend[] };

// ORDER pins the panel order (vault, gcp, aws) regardless of how the
// server lists them. secretBackendSources is the single source of truth.
const ORDER = secretBackendSources;

// SecretBackendsForm renders one independent editor panel per external
// secret backend. Each panel owns its own draft + save/test/delete, so
// there's no shared form state here — this component only sequences the
// panels and renders the intro card. The server always returns all
// three entries; a missing one is tolerated (the panel just won't show).
export function SecretBackendsForm({ initial }: Props) {
  const bySource = useMemo(() => {
    const m = new Map<SecretBackendSource, SecretBackend>();
    for (const b of initial) m.set(b.source, b);
    return m;
  }, [initial]);

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="flex-row items-start gap-3 space-y-0">
          <Info className="mt-0.5 size-5 shrink-0 text-muted-foreground" aria-hidden />
          <div>
            <CardTitle className="text-base">External secret backends</CardTitle>
            <CardDescription>
              Connect HashiCorp Vault, GCP Secret Manager or AWS Secrets
              Manager so pipelines can resolve secret refs at runtime. A saved
              override supersedes the env config and applies immediately — no
              restart required. Credentials are write-only and never echoed
              back.
            </CardDescription>
          </div>
        </CardHeader>
      </Card>

      {ORDER.map((source) => {
        const backend = bySource.get(source);
        if (!backend) return null;
        return <SecretBackendPanel key={source} backend={backend} />;
      })}
    </div>
  );
}
