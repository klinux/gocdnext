import Link from "next/link";
import { notFound } from "next/navigation";
import type { Metadata } from "next";
import { ChevronRight } from "lucide-react";
import { Separator } from "@/components/ui/separator";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { StageSection } from "@/components/runs/stage-section";
import { durationBetween, formatDurationSeconds } from "@/lib/format";
import {
  GocdnextAPIError,
  getRunDetail,
} from "@/server/queries/projects";

type Params = { id: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { id } = await params;
  return { title: `Run ${id.slice(0, 8)} — gocdnext` };
}

export const dynamic = "force-dynamic";

export default async function RunDetailPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { id } = await params;

  let run;
  try {
    run = await getRunDetail(id);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  const totalDuration = formatDurationSeconds(
    durationBetween(run.started_at, run.finished_at),
  );

  const upstream =
    run.cause === "upstream" && run.cause_detail
      ? (run.cause_detail as {
          upstream_run_id?: string;
          upstream_pipeline?: string;
          upstream_stage?: string;
          upstream_run_counter?: number;
        })
      : null;

  return (
    <section className="space-y-6">
      <header>
        <nav aria-label="Breadcrumb" className="text-xs text-muted-foreground">
          <Link href="/" className="hover:text-foreground">
            Projects
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <Link
            href={{
              pathname: "/projects/[slug]",
              query: { slug: run.project_slug },
            }}
            className="hover:text-foreground"
          >
            {run.project_slug}
          </Link>
          <ChevronRight className="mx-1 inline h-3 w-3" aria-hidden />
          <span className="font-mono">
            {run.pipeline_name} #{run.counter}
          </span>
        </nav>

        <div className="mt-2 flex flex-wrap items-center gap-3">
          <h2 className="text-2xl font-semibold tracking-tight">
            {run.pipeline_name}{" "}
            <span className="font-mono text-muted-foreground">#{run.counter}</span>
          </h2>
          <StatusBadge status={run.status} />
        </div>

        <dl className="mt-2 flex flex-wrap gap-x-6 gap-y-1 text-xs text-muted-foreground">
          <Meta k="cause" v={run.cause} />
          <Meta
            k="started"
            v={<RelativeTime at={run.started_at ?? run.created_at} fallback="—" />}
          />
          <Meta k="duration" v={totalDuration} />
          {run.triggered_by ? <Meta k="triggered by" v={run.triggered_by} /> : null}
        </dl>
      </header>

      {upstream ? <UpstreamBanner upstream={upstream} /> : null}

      <Separator />

      <div className="space-y-8">
        {run.stages.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            This run has no stages.
          </p>
        ) : (
          run.stages.map((s) => <StageSection key={s.id} stage={s} />)
        )}
      </div>
    </section>
  );
}

function Meta({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div>
      <dt className="inline text-[10px] uppercase tracking-wide text-muted-foreground/70">
        {k}
      </dt>{" "}
      <dd className="inline font-mono">{v}</dd>
    </div>
  );
}

function UpstreamBanner({
  upstream,
}: {
  upstream: {
    upstream_run_id?: string;
    upstream_pipeline?: string;
    upstream_stage?: string;
    upstream_run_counter?: number;
  };
}) {
  const { upstream_run_id, upstream_pipeline, upstream_stage, upstream_run_counter } =
    upstream;
  return (
    <aside className="rounded-md border border-primary/30 bg-primary/5 px-3 py-2 text-sm">
      Triggered by upstream{" "}
      <span className="font-mono">{upstream_pipeline}</span>
      {typeof upstream_run_counter === "number" ? (
        <span className="font-mono"> #{upstream_run_counter}</span>
      ) : null}
      {upstream_stage ? (
        <>
          {" "}after stage <span className="font-mono">{upstream_stage}</span>{" "}
          passed
        </>
      ) : null}
      {upstream_run_id ? (
        <>
          {" · "}
          <Link
            href={{
              pathname: "/runs/[id]",
              query: { id: upstream_run_id },
            }}
            className="text-primary hover:underline"
          >
            view upstream run
          </Link>
        </>
      ) : null}
    </aside>
  );
}
