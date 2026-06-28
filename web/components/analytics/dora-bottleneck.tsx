import { TriangleAlert } from "lucide-react";

import { fmtDuration } from "@/lib/dora";
import type { LeadTimeBottleneck } from "@/server/queries/analytics";

// The four lead-time stages, in order, with their identity colour and the
// boundary each measures. Colours are a stage-identity palette (Review/Release/
// Deploy reuse the canonical amber/teal tokens; Coding is a distinct steel
// blue with no token equivalent).
type Stage = { label: string; note: string; color: string; sec: number; sample: number };

function stages(b: LeadTimeBottleneck): Stage[] {
  return [
    { label: "Coding", note: "first commit → PR open", color: "#3a6ea5", sec: b.coding_p50_seconds, sample: b.coding_sample },
    { label: "Review", note: "PR open → approval", color: "var(--amber)", sec: b.review_p50_seconds, sample: b.review_sample },
    { label: "Release wait", note: "approval → deploy start", color: "var(--brand-mid)", sec: b.release_wait_p50_seconds, sample: b.release_sample },
    { label: "Deploy", note: "deploy job", color: "var(--teal)", sec: b.deploy_p50_seconds, sample: b.deploy_sample },
  ];
}

// DoraBottleneck shows where lead time is lost: the four stages (p50) as a
// stacked bar + legend, the biggest stage flagged as the top lever, and a
// sample-transparency footer. Renders an info state when no deploy correlates
// to a PR. Pure presentational RSC.
export function DoraBottleneck({ bottleneck }: { bottleneck: LeadTimeBottleneck }) {
  const b = bottleneck;
  const all = stages(b);
  const total = all.reduce((a, s) => a + Math.max(0, s.sec), 0);

  if (b.correlated === 0 || total <= 0) {
    return (
      <div className="rounded-xl bg-card p-5 ring-1 ring-foreground/10">
        <Header total={0} />
        <p className="mt-4 rounded-md border border-dashed p-4 text-center text-xs text-muted-foreground">
          No PR-correlated deploys in this window.
          {b.excluded > 0 ? ` ${b.excluded} excluded (no PR correlation).` : ""}{" "}
          Lead-time decomposition needs deploys whose merged commit matches a
          tracked pull request.
        </p>
      </div>
    );
  }

  const top = all.reduce((m, s) => (s.sec > m.sec ? s : m), all[0]!);
  const topPct = Math.round((top.sec / total) * 100);

  return (
    <div className="rounded-xl bg-card p-5 ring-1 ring-foreground/10">
      <Header total={total} />

      <div className="mt-5 flex h-[30px] gap-0.5 overflow-hidden rounded-md">
        {all.map((s) =>
          s.sec > 0 ? (
            <div
              key={s.label}
              className="flex items-center justify-center font-mono text-[10px] font-semibold whitespace-nowrap text-white/90"
              style={{ flex: s.sec, background: s.color, minWidth: 0 }}
              title={`${s.label}: ${fmtDuration(s.sec)}`}
            >
              {s.sec / total > 0.11 ? s.label : ""}
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
            <span className="ml-auto font-mono tabular-nums">{fmtDuration(s.sec)}</span>
            <span className="w-9 text-right font-mono text-muted-foreground/70 tabular-nums">
              {Math.round((s.sec / total) * 100)}%
            </span>
          </div>
        ))}
      </div>

      <div className="mt-3 flex items-start gap-2 rounded-md bg-status-warning-bg p-2.5 text-xs text-status-warning">
        <TriangleAlert className="mt-px size-3.5 shrink-0" aria-hidden />
        <span>
          <span className="font-mono font-semibold">{top.label}</span> is the
          biggest stage — {topPct}% of lead time. Best lever to ship faster.
        </span>
      </div>

      <p className="mt-2 text-[11px] text-muted-foreground/70">
        p50 across {b.correlated} PR-correlated deploys
        {b.excluded > 0 ? ` · ${b.excluded} excluded (no PR)` : ""}
        {b.review_sample < b.correlated ? ` · Review from ${b.review_sample} with an approval` : ""}.
      </p>
    </div>
  );
}

function Header({ total }: { total: number }) {
  return (
    <div className="flex items-start justify-between gap-4">
      <div>
        <div className="text-sm font-semibold">Where lead time is lost</div>
        <div className="text-xs text-muted-foreground">
          Commit → production, decomposed (p50)
        </div>
      </div>
      {total > 0 ? (
        <div className="text-right">
          <div className="text-xl font-semibold tabular-nums">{fmtDuration(total)}</div>
          <div className="font-mono text-[10.5px] uppercase tracking-wide text-muted-foreground/70">
            commit → deploy
          </div>
        </div>
      ) : null}
    </div>
  );
}
