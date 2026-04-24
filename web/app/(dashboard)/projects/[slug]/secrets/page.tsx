import Link from "next/link";
import { notFound } from "next/navigation";
import type { Metadata, Route } from "next";
import { Lock } from "lucide-react";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { SecretDialog } from "@/components/secrets/secret-dialog.client";
import { DeleteSecretButton } from "@/components/secrets/delete-secret-button.client";
import { Button } from "@/components/ui/button";
import { Toaster } from "@/components/ui/sonner";
import {
  GocdnextAPIError,
  getProjectDetail,
  listSecrets,
} from "@/server/queries/projects";

type Params = { slug: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `Secrets — ${slug}` };
}

export const dynamic = "force-dynamic";

export default async function SecretsPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { slug } = await params;

  // 404 early if the project is missing — avoids the secrets fetch returning
  // a useless 503/404 chain.
  try {
    await getProjectDetail(slug, 1);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  let data;
  try {
    data = await listSecrets(slug);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 503) {
      return <SubsystemDisabled slug={slug} />;
    }
    throw err;
  }
  const secrets = data.secrets;
  const inherited = data.inherited ?? [];

  return (
    <section className="space-y-6">
      <Toaster position="top-right" richColors />
      <div className="flex items-start justify-between gap-4">
        <p className="text-sm text-muted-foreground">
          Encrypted at rest and never echoed back. Reference by name from a job&apos;s{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">secrets:</code> list.
        </p>
        <SecretDialog slug={slug} />
      </div>

      {secrets.length === 0 ? (
        <EmptyState slug={slug} />
      ) : (
        <div className="overflow-hidden rounded-lg border border-border bg-card">
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
                        slug={slug}
                        mode="rotate"
                        name={s.name}
                        trigger={
                          <Button variant="ghost" size="sm">
                            Rotate
                          </Button>
                        }
                      />
                      <DeleteSecretButton slug={slug} name={s.name} />
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {inherited.length > 0 ? (
        <section className="space-y-3">
          <div>
            <h3 className="text-lg font-semibold tracking-tight">
              Inherited from global scope
            </h3>
            <p className="mt-1 text-sm text-muted-foreground">
              Available to every pipeline here unless a same-name secret is
              defined above. Editing lives in{" "}
              <Link
                href={"/settings/secrets" as Route}
                className="underline underline-offset-4 hover:text-foreground"
              >
                /settings/secrets
              </Link>{" "}
              (admins only).
            </p>
          </div>
          <div className="overflow-hidden rounded-lg border border-border bg-card">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead>Updated</TableHead>
                  <TableHead className="text-right">Scope</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {inherited.map((s) => (
                  <TableRow key={`inh-${s.name}`}>
                    <TableCell className="font-mono">{s.name}</TableCell>
                    <TableCell className="text-muted-foreground">
                      <RelativeTime at={s.created_at} />
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      <RelativeTime at={s.updated_at} />
                    </TableCell>
                    <TableCell className="text-right">
                      <span className="inline-flex items-center rounded-full border border-border bg-muted/40 px-2 py-0.5 text-[11px] font-medium text-muted-foreground">
                        global
                      </span>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </section>
      ) : null}
    </section>
  );
}

function EmptyState({ slug }: { slug: string }) {
  return (
    <section className="mx-auto max-w-lg rounded-lg border border-dashed border-border p-10 text-center">
      <Lock className="mx-auto h-6 w-6 text-muted-foreground" aria-hidden />
      <h3 className="mt-3 text-lg font-semibold">No secrets yet</h3>
      <p className="mt-2 text-sm text-muted-foreground">
        Click <strong>New secret</strong> above, or run{" "}
        <code className="rounded bg-muted px-1 py-0.5 text-xs">
          gocdnext secret set --slug {slug} NAME
        </code>{" "}
        from a terminal.
      </p>
    </section>
  );
}

function SubsystemDisabled({ slug }: { slug: string }) {
  return (
    <section className="mx-auto max-w-lg rounded-lg border border-destructive/40 bg-destructive/5 p-10 text-center">
      <h3 className="text-lg font-semibold">Secrets subsystem not configured</h3>
      <p className="mt-2 text-sm text-muted-foreground">
        The server was started without{" "}
        <code className="rounded bg-muted px-1 py-0.5 text-xs">GOCDNEXT_SECRET_KEY</code>.
        Secrets for <span className="font-mono">{slug}</span> can&apos;t be created or
        listed until the key is set and the server restarted.
      </p>
    </section>
  );
}
