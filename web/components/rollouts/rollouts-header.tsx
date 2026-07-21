import type { ReactNode } from "react";
import { Rocket } from "lucide-react";

type Props = { right?: ReactNode };

// RolloutsHeader is the shared page head (title + subtitle) with an optional
// right slot. Presentational and hook-free so both the RSC page (selector /
// access states) and the client live view can render it. The live view fills
// `right` with the pulsing live indicator + refresh.
export function RolloutsHeader({ right }: Props) {
  return (
    <div className="flex flex-wrap items-end gap-4">
      <div>
        <div className="flex items-center gap-3">
          <Rocket className="size-5 text-teal-500" aria-hidden />
          <h1 className="text-xl font-bold tracking-tight sm:text-2xl">
            Rollouts
          </h1>
        </div>
        <p className="mt-1 max-w-2xl text-sm text-muted-foreground">
          Progressive delivery via argo-rollouts — watch the canary steps,
          traffic split and analysis before 100% of traffic is exposed.
        </p>
      </div>
      {right ? (
        <div className="ml-auto flex items-center gap-2.5">{right}</div>
      ) : null}
    </div>
  );
}
