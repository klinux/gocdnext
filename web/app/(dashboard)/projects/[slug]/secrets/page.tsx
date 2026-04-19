import Link from "next/link";
import { notFound } from "next/navigation";
import type { Metadata } from "next";
import { ChevronRight, Lock } from "lucide-react";
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

  let secrets;
  try {
    secrets = await listSecrets(slug);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 503) {
      return <SubsystemDisabled slug={slug} />;
    }
    throw err;
  }

  return (
    <section className="space-y-6">
      <Toaster position="top-right" richColors />
      <header>
        <nav aria-label="Breadcrumb" className="text-xs text-muted-foreground">
          <Link href="/" className="hover:text-foreground">
            Projects
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <Link
            href={{ pathname: "/projects/[slug]", query: { slug } }}
            className="hover:text-foreground"
          >
            {slug}
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <span>secrets</span>
        </nav>
        <div className="mt-2 flex items-baseline justify-between">
          <h2 className="text-2xl font-semibold tracking-tight">Secrets</h2>
          <SecretDialog slug={slug} />
        </div>
        <p className="mt-1 text-sm text-muted-foreground">
          Encrypted at rest and never echoed back. Reference by name from a job&apos;s{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">secrets:</code> list.
        </p>
      </header>

      {secrets.length === 0 ? (
        <EmptyState slug={slug} />
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
      )}
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
