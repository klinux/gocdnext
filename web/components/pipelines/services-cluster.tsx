import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { ServiceGlyph, detectTech } from "@/components/shared/service-tech";
import type { StatusTone } from "@/lib/status";

// ServicesCluster renders a pipeline's declared services as a violet
// bracket of per-tech tiles before the stage track — services are
// environment containers (DB/cache/broker), not jobs: they boot before
// the jobs and stay alive for the whole run, so they get their own colour
// (violet) distinct from jobs/stages (teal). Recreated from the services
// design handoff.
//
// The tech is detected from the service NAME (no extra data fetch); the
// readiness dot is an approximation derived from the run tone (the list
// stays fetch-free — precise per-service ready/booting/failed lives in the
// run's Services detail).

// dotColor approximates per-service readiness from the run tone: a
// finished/running run → ready, a waiting run → booting, a failed run →
// failed, otherwise muted.
function dotColor(tone: StatusTone): string {
  switch (tone) {
    case "success":
    case "running":
      return "#3fb950"; // ready
    case "queued":
    case "awaiting":
      return "#d9a429"; // booting
    case "failed":
      return "#f85149"; // failed
    default:
      return "#6e7681"; // muted (canceled/skipped/warning/neutral)
  }
}

function Tile({ name, dot }: { name: string; dot: string }) {
  return (
    <span className="relative">
      <ServiceGlyph tech={detectTech(name)} className="size-[22px]" />
      <span
        className="absolute -bottom-px -right-px size-[6px] rounded-full ring-2 ring-card"
        style={{ background: dot }}
        aria-hidden
      />
    </span>
  );
}

// Cap the tiles so the cluster + connector stay inside the fixed lane
// (keeps the stage circles aligned); overflow collapses to "+N".
const MAX_TILES = 3;

export function ServicesCluster({
  names,
  tone,
}: {
  names: string[];
  tone: StatusTone;
}) {
  if (names.length === 0) return null;
  const dot = dotColor(tone);
  const shown = names.slice(0, MAX_TILES);
  const extra = names.length - shown.length;
  return (
    <div className="flex items-start gap-0">
      <Tooltip>
        <TooltipTrigger
          render={<div className="flex cursor-help flex-col items-center gap-1" />}
        >
          <div
            className="flex items-center gap-[3px] rounded-[9px] border px-[7px] py-1"
            style={{
              background: "rgba(167,121,233,.09)",
              borderColor: "rgba(167,121,233,.32)",
            }}
          >
            {shown.map((n, i) => (
              <Tile key={`${n}-${i}`} name={n} dot={dot} />
            ))}
            {extra > 0 ? (
              <span
                className="px-0.5 font-mono text-[9px] font-semibold"
                style={{ color: "#9d7fd0" }}
              >
                +{extra}
              </span>
            ) : null}
          </div>
          <span
            className="font-mono text-[8.5px] font-semibold uppercase tracking-wide"
            style={{ color: "#9d7fd0" }}
          >
            services
          </span>
        </TooltipTrigger>
        <TooltipContent>
          Services (environment): {names.join(", ")} — start before the jobs
          and stay alive for the whole run
        </TooltipContent>
      </Tooltip>
      {/* dashed violet connector linking the cluster to the first job */}
      <span
        className="mt-[10px] ml-1 w-4 border-t border-dashed"
        style={{ borderColor: "rgba(167,121,233,.4)" }}
        aria-hidden
      />
    </div>
  );
}
