import { TriangleAlert } from "lucide-react";

import { fmtDuration } from "@/lib/dora";
import type { LeadTimeBottleneck } from "@/server/queries/analytics";

// The four lead-time stages, in order, with their identity colour, the boundary
// each measures, the stage p50, and its own sample. Colours are a stage-identity
// palette (Review/Release/Deploy reuse the canonical amber/teal tokens; Coding
// is a distinct steel blue with no token equivalent).
type Stage = { label: string; note: string; color: string; sec: number; sample: number };

function stages(b: LeadTimeBottleneck): Stage[] {
  return [
    { label: "Coding", note: "first commit → PR open", color: "#3a6ea5", sec: b.coding_p50_seconds, sample: b.coding_sample },
    { label: "Review", note: "PR open → approval", color: "var(--amber)", sec: b.review_p50_seconds, sample: b.review_sample },
    { label: "Release wait", note: "approval → deploy start", color: "var(--brand-mid)", sec: b.release_wait_p50_seconds, sample: b.release_sample },
    { label: "Deploy", note: "deploy job", color: "var(--teal)", sec: b.deploy_p50_seconds, sample: b.deploy_sample },
  ];
}

// DoraBottleneck shows where lead time is lost. The header is the TRUE
// end-to-end p50 (commit→deploy); the stacked bar is the per-stage split (each
// an independent median, so the segments need not sum to the header). The
// biggest stage is flagged, and each stage carries its own sample.
export function DoraBottleneck({ bottleneck }: { bottleneck: LeadTimeBottleneck }) {
  const b = bottleneck;
  const all = stages(b);
  const barTotal = all.reduce((a, s) => a + Math.max(0, s.sec), 0);

  if (b.correlated === 0) {
    return (
      <Shell totalP50={0}>
        <Info>
          No PR-correlated deploys in this window.
          {b.excluded > 0 ? ` ${b.excluded} excluded (no PR correlation).` : ""}{" "}
          Decomposition needs deploys whose merged commit matches a tracked pull
          request.
        </Info>
      </Shell>
    );
  }
  if (barTotal <= 0) {
    return (
      <Shell totalP50={b.total_p50_seconds}>
        <Info>
          {b.correlated} PR-correlated deploy{b.correlated === 1 ? "" : "s"}, but
          no stage timings yet — the matched PRs are missing lifecycle timestamps
          (enable the Pull requests + Pull request reviews webhooks).
        </Info>
      </Shell>
    );
  }

  const top = all.reduce((m, s) => (s.sec > m.sec ? s : m), all[0]!);
  const topPct = Math.round((top.sec / barTotal) * 100);

  return (
    <Shell totalP50={b.total_p50_seconds}>
      <div className="mt-5 flex h-[30px] gap-0.5 overflow-hidden rounded-md">
        {all.map((s) =>
          s.sec > 0 ? (
            <div
              key={s.label}
              className="flex items-center justify-center font-mono text-[10px] font-semibold whitespace-nowrap text-white/90"
              style={{ flex: s.sec, background: s.color, minWidth: 0 }}
              title={`${s.label}: ${fmtDuration(s.sec)} (n ${s.sample})`}
            >
              {s.sec / barTotal > 0.11 ? s.label : ""}
            </div>
          ) : null,
        )}
      </div>

      <div className="mt-3 space-y-1.5">
        {all.map((s) => (
          <div key={s.label} className="flex items-center gap-2 text-xs">
            <span className="size-2.5 shrink-0 rounded-sm" style={{ background: s.color }} />
            <span className="font-medium">{s.label}</span>
            <span className="text-muted-foreground/70">{s.note}</span>
            {s.sample < b.correlated ? (
              <span className="font-mono text-[10px] text-muted-foreground/60">n {s.sample}</span>
            ) : null}
            <span className="ml-auto font-mono tabular-nums">{fmtDuration(s.sec)}</span>
            <span className="w-9 text-right font-mono text-muted-foreground/70 tabular-nums">
              {Math.round((s.sec / barTotal) * 100)}%
            </span>
          </div>
        ))}
      </div>

      <div className="mt-3 flex items-start gap-2 rounded-md bg-status-warning-bg p-2.5 text-xs text-status-warning">
        <TriangleAlert className="mt-px size-3.5 shrink-0" aria-hidden />
        <span>
          <span className="font-mono font-semibold">{top.label}</span> is the
          biggest stage — {topPct}% of the decomposed time. Best lever to ship
          faster.
        </span>
      </div>

      <p className="mt-2 text-[11px] text-muted-foreground/70">
        Stage medians across {b.correlated} PR-correlated deploys
        {b.excluded > 0 ? ` · ${b.excluded} excluded (no PR)` : ""}. Stages are
        independent samples (n), so they may not sum to the lead-time p50.
      </p>
    </Shell>
  );
}

function Shell({ totalP50, children }: { totalP50: number; children: React.ReactNode }) {
  return (
    <div className="rounded-xl bg-card p-5 ring-1 ring-foreground/10">
      <div className="flex items-start justify-between gap-4">
        <div>
          <div className="text-sm font-semibold">Where lead time is lost</div>
          <div className="text-xs text-muted-foreground">
            Commit → deploy, decomposed
          </div>
        </div>
        {totalP50 > 0 ? (
          <div className="text-right">
            <div className="text-xl font-semibold tabular-nums">{fmtDuration(totalP50)}</div>
            <div className="font-mono text-[10.5px] uppercase tracking-wide text-muted-foreground/70">
              lead time · p50
            </div>
          </div>
        ) : null}
      </div>
      {children}
    </div>
  );
}

function Info({ children }: { children: React.ReactNode }) {
  return (
    <p className="mt-4 rounded-md border border-dashed p-4 text-center text-xs text-muted-foreground">
      {children}
    </p>
  );
}
