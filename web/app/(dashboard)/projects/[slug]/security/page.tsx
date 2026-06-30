import { notFound } from "next/navigation";
import type { Metadata } from "next";
import { ShieldAlert } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { FindingsTable } from "@/components/security/findings-table";
import { Pagination } from "@/components/shared/pagination";
import { StatusPill } from "@/components/shared/status-pill";
import { SEVERITY_ORDER, severityLabel, severityTone } from "@/lib/severity";
import { GocdnextAPIError, listFindings } from "@/server/queries/projects";
import type { FixedFinding } from "@/types/api";

type Params = { slug: string };
type Search = {
  severity?: string;
  tool?: string;
  rule?: string;
  offset?: string;
};

const PAGE_SIZE = 50;

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { slug } = await params;
  return { title: `Security — ${slug}` };
}

// Findings come from the latest run per pipeline + reflect the most recent
// scan, so always render live.
export const dynamic = "force-dynamic";

export default async function SecurityPage({
  params,
  searchParams,
}: {
  params: Promise<Params>;
  searchParams: Promise<Search>;
}) {
  const { slug } = await params;
  const sp = await searchParams;
  const severity = sp.severity ?? "";
  const tool = sp.tool ?? "";
  const rule = sp.rule ?? "";
  const offset = Math.max(0, Number(sp.offset ?? 0) || 0);

  let data;
  try {
    data = await listFindings(slug, {
      severity: severity || undefined,
      tool: tool || undefined,
      rule: rule || undefined,
      limit: PAGE_SIZE,
      offset,
    });
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }

  const basePath = `/projects/${slug}/security`;

  return (
    <section className="space-y-5">
      <header className="space-y-3">
        <p className="max-w-[820px] text-sm text-muted-foreground">
          Vulnerability findings ingested from your scanners&apos; SARIF
          artifacts (semgrep, trivy, osv-scanner, gitleaks…), sourced from the
          latest run of each pipeline.
        </p>
        <div className="flex flex-wrap gap-2">
          {SEVERITY_ORDER.map((sev) => (
            <StatusPill key={sev} tone={severityTone(sev)}>
              {data.severity_counts[sev] ?? 0} {severityLabel(sev)}
            </StatusPill>
          ))}
        </div>
        {data.fixed_total > 0 ? <FixedSummary fixed={data.fixed} total={data.fixed_total} /> : null}
      </header>

      <form
        method="GET"
        action={basePath}
        className="flex flex-wrap items-end gap-3 rounded-lg border border-border bg-card p-3"
      >
        <div className="grid gap-1">
          <Label htmlFor="sev" className="text-xs">
            Severity
          </Label>
          <select
            id="sev"
            name="severity"
            defaultValue={severity}
            className="h-9 rounded-md border border-input bg-background px-2 text-sm"
          >
            <option value="">All</option>
            {SEVERITY_ORDER.map((sev) => (
              <option key={sev} value={sev}>
                {severityLabel(sev)}
              </option>
            ))}
          </select>
        </div>
        <div className="grid gap-1">
          <Label htmlFor="tool" className="text-xs">
            Tool
          </Label>
          <Input id="tool" name="tool" defaultValue={tool} placeholder="Trivy" className="h-9 w-40 text-sm" />
        </div>
        <div className="grid gap-1">
          <Label htmlFor="rule" className="text-xs">
            Rule
          </Label>
          <Input id="rule" name="rule" defaultValue={rule} placeholder="CVE-…" className="h-9 w-48 text-sm" />
        </div>
        <Button type="submit" size="sm">
          Filter
        </Button>
        {severity || tool || rule ? (
          <Button type="button" variant="ghost" size="sm" render={<a href={basePath} />}>
            Clear
          </Button>
        ) : null}
      </form>

      {data.findings.length === 0 ? (
        <EmptyState filtered={!!(severity || tool || rule)} />
      ) : (
        <>
          <FindingsTable findings={data.findings} />
          <Pagination
            offset={offset}
            total={data.total}
            pageSize={PAGE_SIZE}
            basePath={basePath}
            params={{ severity, tool, rule }}
          />
        </>
      )}
    </section>
  );
}

// FixedSummary surfaces identities that were present in a prior scan but are
// gone from the scanner's latest run — collapsed so it never competes with the
// open findings, expandable for the "what got resolved" view.
function FixedSummary({ fixed, total }: { fixed: FixedFinding[]; total: number }) {
  return (
    <details className="rounded-md border border-border bg-card px-3 py-2 text-sm">
      <summary className="cursor-pointer font-medium text-muted-foreground">
        ✓ {total} fixed since last scan
      </summary>
      <ul className="mt-2 space-y-1.5">
        {fixed.map((f) => (
          <li key={f.id} className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
            <StatusPill tone={severityTone(f.severity)}>{severityLabel(f.severity)}</StatusPill>
            <span className="font-mono">{f.rule_id}</span>
            <span>{f.tool}</span>
            {f.location_path ? (
              <span className="font-mono">
                {f.location_path}
                {f.location_line ? `:${f.location_line}` : ""}
              </span>
            ) : null}
          </li>
        ))}
      </ul>
    </details>
  );
}

function EmptyState({ filtered }: { filtered: boolean }) {
  if (filtered) {
    return (
      <p className="rounded-md border border-dashed p-8 text-center text-sm text-muted-foreground">
        No findings match these filters.
      </p>
    );
  }
  return (
    <p className="rounded-md border border-dashed p-8 text-center text-sm text-muted-foreground">
      No security findings yet. Add a scanner job that emits a{" "}
      <span className="font-mono">.sarif</span> artifact (semgrep, trivy,
      osv-scanner, gitleaks) — findings appear here after its next run.
    </p>
  );
}
