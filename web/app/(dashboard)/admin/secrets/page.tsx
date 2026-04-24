import type { Metadata } from "next";
import { KeyRound, Lock } from "lucide-react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Toaster } from "@/components/ui/sonner";
import { RelativeTime } from "@/components/shared/relative-time";
import { SecretDialog } from "@/components/secrets/secret-dialog.client";
import { DeleteSecretButton } from "@/components/secrets/delete-secret-button.client";
import { listGlobalSecrets } from "@/server/queries/admin";

export const metadata: Metadata = { title: "Global secrets — gocdnext" };

export const dynamic = "force-dynamic";

// Global secrets are admin-only at the API layer. The /settings
// shell already gates on role=admin before this page renders, so
// additional client-side checks would just duplicate the guard.
export default async function GlobalSecretsPage() {
  let secrets;
  try {
    secrets = await listGlobalSecrets();
  } catch (err) {
    // 503 from the API means GOCDNEXT_SECRET_KEY is unset. Surface
    // that specifically — otherwise the operator would see a
    // generic "something went wrong" and not know they need to
    // restart the control plane with the key.
    const message = err instanceof Error ? err.message : String(err);
    if (message.includes("503")) {
      return <SubsystemDisabled />;
    }
    throw err;
  }

  return (
    <section className="space-y-6">
      <Toaster position="top-right" richColors />
      <header className="flex items-baseline justify-between">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
            <KeyRound className="h-6 w-6" aria-hidden />
            Global secrets
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Available to every pipeline. A project secret with the same name
            takes precedence — useful for overriding a shared default.
          </p>
        </div>
        <SecretDialog scope="global" />
      </header>

      {secrets.length === 0 ? (
        <EmptyState />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Updated</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {secrets.map((s) => (
              <TableRow key={s.name}>
                <TableCell className="font-mono">{s.name}</TableCell>
                <TableCell className="text-muted-foreground">
                  <RelativeTime at={s.created_at} />
                </TableCell>
                <TableCell className="text-muted-foreground">
                  <RelativeTime at={s.updated_at} />
                </TableCell>
                <TableCell className="text-right">
                  <div className="inline-flex items-center gap-1">
                    <SecretDialog
                      scope="global"
                      mode="rotate"
                      name={s.name}
                      trigger={
                        <Button variant="ghost" size="sm">
                          Rotate
                        </Button>
                      }
                    />
                    <DeleteSecretButton scope="global" name={s.name} />
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}

function EmptyState() {
  return (
    <section className="mx-auto max-w-lg rounded-lg border border-dashed border-border p-10 text-center">
      <Lock className="mx-auto h-6 w-6 text-muted-foreground" aria-hidden />
      <h3 className="mt-3 text-lg font-semibold">No global secrets yet</h3>
      <p className="mt-2 text-sm text-muted-foreground">
        Click <strong>New secret</strong> to add one. Every pipeline on this
        server can reference it from a job&apos;s{" "}
        <code className="rounded bg-muted px-1 py-0.5 text-xs">secrets:</code>{" "}
        list.
      </p>
    </section>
  );
}

function SubsystemDisabled() {
  return (
    <section className="mx-auto max-w-lg rounded-lg border border-destructive/40 bg-destructive/5 p-10 text-center">
      <h3 className="text-lg font-semibold">Secrets subsystem not configured</h3>
      <p className="mt-2 text-sm text-muted-foreground">
        The server was started without{" "}
        <code className="rounded bg-muted px-1 py-0.5 text-xs">
          GOCDNEXT_SECRET_KEY
        </code>
        . Global secrets can&apos;t be created or listed until the key is set
        and the server restarted.
      </p>
    </section>
  );
}
