import { fmtFreq } from "@/lib/dora";
import type { DoraDay } from "@/server/queries/analytics";

// One plotted day. `fail` uses the API's DORA semantics — deploys_failed =
// status='failed' OR is_rollback — so a successful rollback counts red, like
// the CFR card. `ok` is total − fail, keeping the stack height = the day's
// total terminal deploys.
type Day = { ok: number; fail: number };

const CHART_PX = 120; // tallest bar in px; per-deploy unit derives from the max.

function toDays(daily: DoraDay[]): Day[] {
  return daily.map((d) => ({
    ok: Math.max(0, d.deploys_total - d.deploys_failed),
    fail: d.deploys_failed,
  }));
}

// DoraDeployFrequency plots deploys-to-production per day over the window as
// stacked bars — successful (teal) with failures (red) on top — so cadence and
// failure clustering read at a glance. Heights are in px (CHART_PX / busiest
// day), matching the handoff. Pure presentational RSC.
export function DoraDeployFrequency({
  daily,
  windowDays,
  freqPerDay,
}: {
  daily: DoraDay[];
  windowDays: number;
  freqPerDay: number;
}) {
  const days = toDays(daily);
  const maxTotal = Math.max(1, ...days.map((d) => d.ok + d.fail));
  const unit = CHART_PX / maxTotal;
  const totalDeploys = days.reduce((a, d) => a + d.ok + d.fail, 0);
  const totalFails = days.reduce((a, d) => a + d.fail, 0);

  return (
    <div className="rounded-xl bg-card p-5 ring-1 ring-foreground/10">
      <div className="flex items-start justify-between gap-4">
        <div>
          <div className="text-sm font-semibold">Deploy frequency</div>
          <div className="text-xs text-muted-foreground">
            Deploys per day · change failures highlighted in red
          </div>
        </div>
        <div className="text-right">
          <div className="text-xl font-semibold tabular-nums">
            {fmtFreq(freqPerDay, "day")}
          </div>
          <div className="font-mono text-[10.5px] uppercase tracking-wide text-muted-foreground/70">
            avg · {windowDays}d
          </div>
        </div>
      </div>

      <div className="mt-5 flex items-end gap-[3px]" style={{ height: CHART_PX + 12 }}>
        {days.map((d, i) => (
          <div
            key={i}
            className="flex flex-1 flex-col-reverse"
            style={{ minHeight: 2 }}
            title={`${d.ok} ok${d.fail ? `, ${d.fail} failed` : ""}`}
          >
            {d.ok > 0 ? (
              <div
                className="rounded-b-sm"
                style={{ height: d.ok * unit, background: "var(--teal)", opacity: 0.8 }}
              />
            ) : null}
            {d.fail > 0 ? (
              <div
                className="rounded-sm"
                style={{ height: d.fail * unit, background: "var(--red)" }}
              />
            ) : null}
          </div>
        ))}
      </div>

      <div className="mt-2 flex justify-between font-mono text-[10.5px] text-muted-foreground/70">
        <span>{windowDays}d ago</span>
        <span>today</span>
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-2 text-xs">
        <span className="inline-flex items-center gap-1.5">
          <span className="size-2.5 rounded-sm" style={{ background: "var(--teal)", opacity: 0.8 }} />
          Deploy ok
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="size-2.5 rounded-sm" style={{ background: "var(--red)" }} />
          Change failure
        </span>
        <span className="basis-full text-muted-foreground/70 sm:ml-auto sm:basis-auto">
          {totalFails} change failures in {totalDeploys} deploys
        </span>
      </div>
    </div>
  );
}
