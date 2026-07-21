import { shortHash, trafficSplit } from "@/lib/rollouts";

type Props = {
  canaryWeight: number;
  stableHash: string;
  podHash: string;
};

// TrafficBar draws the stable/canary traffic split derived from canary_weight.
// The bar carries an accessible label with both shares; the segment widths are
// inline (percent) so the ratio survives a token-only palette. A muted fill
// stands in for the handoff's diagonal hachure (kept token-based, no raw hex).
export function TrafficBar({ canaryWeight, stableHash, podHash }: Props) {
  const { canary, stable } = trafficSplit(canaryWeight);
  return (
    <div>
      <div
        role="img"
        aria-label={`Traffic split: canary ${canary}%, stable ${stable}%`}
        className="flex h-3 overflow-hidden rounded-md border border-border"
      >
        <div
          aria-hidden
          className="h-full bg-muted-foreground/25 transition-[width] duration-300"
          style={{ width: `${stable}%` }}
        />
        <div
          aria-hidden
          className="h-full bg-gradient-to-r from-teal-600 to-teal-400 transition-[width] duration-300"
          style={{ width: `${canary}%` }}
        />
      </div>
      <div className="mt-2 flex flex-wrap gap-x-5 gap-y-1 font-mono text-[11px] text-muted-foreground">
        <span className="flex items-center gap-2">
          <span
            className="size-3 rounded-sm bg-muted-foreground/25"
            aria-hidden
          />
          stable {shortHash(stableHash)}
        </span>
        <span className="flex items-center gap-2">
          <span
            className="size-3 rounded-sm bg-gradient-to-r from-teal-600 to-teal-400"
            aria-hidden
          />
          canary {shortHash(podHash)}
        </span>
      </div>
    </div>
  );
}
