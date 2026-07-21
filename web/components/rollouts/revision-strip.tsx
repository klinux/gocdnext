import { ArrowRight, CheckCircle2, Rocket } from "lucide-react";

import { cn } from "@/lib/utils";
import { imageParts, shortHash, trafficSplit } from "@/lib/rollouts";
import type { Rollout } from "@/types/api";

type CardProps = {
  role: "stable" | "canary";
  hash: string;
  pct: number;
  caption: string;
  image?: { name: string; tag?: string };
};

function RevCard({ role, hash, pct, caption, image }: CardProps) {
  const canary = role === "canary";
  return (
    <div
      className={cn(
        "flex flex-col gap-3 rounded-xl border bg-muted/20 p-4",
        canary ? "border-teal-500/40" : "border-border",
      )}
    >
      <div className="flex items-center justify-between gap-2">
        <span
          className={cn(
            "inline-flex items-center gap-2 font-mono text-[10.5px] font-bold uppercase tracking-wide",
            canary ? "text-teal-600 dark:text-teal-400" : "text-muted-foreground",
          )}
        >
          {canary ? (
            <Rocket className="size-3.5" aria-hidden />
          ) : (
            <CheckCircle2 className="size-3.5" aria-hidden />
          )}
          {canary ? "Canary" : "Stable"}
        </span>
        <span className="font-mono text-[11px] font-semibold text-muted-foreground">
          {shortHash(hash)}
        </span>
      </div>

      {image ? (
        <div className="font-mono text-[13px] font-semibold text-foreground">
          {image.name}
          {image.tag ? (
            <span className="text-teal-600 dark:text-teal-400">:{image.tag}</span>
          ) : null}
        </div>
      ) : (
        <div className="font-mono text-[13px] text-muted-foreground">
          current stable replica set
        </div>
      )}

      <div className="mt-auto flex items-baseline gap-2">
        <span
          className={cn(
            "font-mono text-2xl font-bold tracking-tight tabular-nums",
            canary ? "text-teal-600 dark:text-teal-400" : "text-foreground",
          )}
        >
          {pct}%
        </span>
        <span className="text-[11px] text-muted-foreground">{caption}</span>
      </div>
    </div>
  );
}

type Props = { rollout: Rollout };

// RevisionStrip is the stable-vs-canary pair with an arrow between. The API
// exposes a single rollout `image` (the desired/canary image) plus the two
// pod-template hashes — so only the canary card shows the image+tag; the stable
// card is identified by its hash. Fidelity note for the handoff, which had a
// per-revision commit line the read API does not carry.
export function RevisionStrip({ rollout }: Props) {
  const { canary, stable } = trafficSplit(rollout.canary_weight);
  const image = imageParts(rollout.image);
  return (
    <div className="grid grid-cols-1 items-stretch gap-3 sm:grid-cols-[1fr_2.5rem_1fr]">
      <RevCard
        role="stable"
        hash={rollout.stable_hash}
        pct={stable}
        caption="of production traffic"
      />
      <div
        className="hidden items-center justify-center text-muted-foreground sm:flex"
        aria-hidden
      >
        <ArrowRight className="size-5" />
      </div>
      <RevCard
        role="canary"
        hash={rollout.pod_hash}
        pct={canary}
        caption="receiving now"
        image={image.name ? image : undefined}
      />
    </div>
  );
}
