import { TIER_COLOR } from "@/lib/dora";

// Static DORA benchmark thresholds — the reference the tiers are derived from
// (mirrors classification in @/lib/dora). Elite cells are tinted teal.
const ROWS: { metric: string; cells: [string, string, string, string] }[] = [
  { metric: "Deploy frequency", cells: ["on-demand", "1×day–1×wk", "1×wk–1×mo", "< 1×mo"] },
  { metric: "Lead time", cells: ["< 1 day", "1 day–1 wk", "1 wk–1 mo", "> 1 mo"] },
  { metric: "Change failure rate", cells: ["0–15%", "16–30%", "31–45%", "> 45%"] },
  { metric: "Time to restore", cells: ["< 1 hour", "< 1 day", "< 1 wk", "> 1 wk"] },
];

const HEADERS: { label: string; color: string }[] = [
  { label: "Elite", color: TIER_COLOR.elite },
  { label: "High", color: TIER_COLOR.high },
  { label: "Medium", color: TIER_COLOR.medium },
  { label: "Low", color: TIER_COLOR.low },
];

export function DoraBenchmark() {
  return (
    <div className="rounded-xl bg-card p-4 ring-1 ring-foreground/10">
      <div className="mb-3 font-mono text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
        DORA benchmark reference
      </div>
      <div className="grid grid-cols-[150px_repeat(4,1fr)] gap-px overflow-hidden rounded-md bg-border text-xs">
        <div className="bg-card px-3 py-2" />
        {HEADERS.map((h) => (
          <div
            key={h.label}
            className="bg-card px-3 py-2 font-mono text-[11px] font-semibold uppercase tracking-wide"
            style={{ color: h.color }}
          >
            {h.label}
          </div>
        ))}
        {ROWS.map((r) => (
          <div key={r.metric} className="contents">
            <div className="bg-card px-3 py-2 text-muted-foreground">{r.metric}</div>
            {r.cells.map((c, i) => (
              <div
                key={c}
                className="bg-card px-3 py-2 font-mono text-foreground"
                style={i === 0 ? { color: TIER_COLOR.elite } : undefined}
              >
                {c}
              </div>
            ))}
          </div>
        ))}
      </div>
    </div>
  );
}
