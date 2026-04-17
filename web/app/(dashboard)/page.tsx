import Link from "next/link";
import type { Metadata } from "next";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { RelativeTime } from "@/components/shared/relative-time";
import { listProjects } from "@/server/queries/projects";

export const metadata: Metadata = {
  title: "Projects — gocdnext",
};

export const dynamic = "force-dynamic";

export default async function ProjectsPage() {
  const projects = await listProjects();

  if (projects.length === 0) {
    return <EmptyState />;
  }

  return (
    <section className="space-y-6">
      <header className="flex items-baseline justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Projects</h2>
          <p className="text-sm text-muted-foreground">
            {projects.length} project{projects.length === 1 ? "" : "s"} registered.
          </p>
        </div>
      </header>

      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {projects.map((p) => (
          <Link
            key={p.id}
            href={{ pathname: "/projects/[slug]", query: { slug: p.slug } }}
            className="group"
          >
            <Card className="h-full transition-colors group-hover:border-primary/40">
              <CardHeader>
                <CardTitle className="truncate">{p.name}</CardTitle>
                <CardDescription className="truncate">{p.slug}</CardDescription>
              </CardHeader>
              <CardContent className="flex items-end justify-between text-sm text-muted-foreground">
                <span>
                  {p.pipeline_count} pipeline{p.pipeline_count === 1 ? "" : "s"}
                </span>
                <span>
                  latest run <RelativeTime at={p.latest_run_at} />
                </span>
              </CardContent>
            </Card>
          </Link>
        ))}
      </div>
    </section>
  );
}

function EmptyState() {
  return (
    <section className="mx-auto max-w-lg rounded-lg border border-dashed border-border p-10 text-center">
      <h2 className="text-xl font-semibold">No projects yet</h2>
      <p className="mt-2 text-sm text-muted-foreground">
        Run <code className="rounded bg-muted px-1 py-0.5">gocdnext apply --slug my-project .</code>{" "}
        from a repo with a <code className="rounded bg-muted px-1 py-0.5">.gocdnext/</code> folder
        to register your first pipeline.
      </p>
    </section>
  );
}
