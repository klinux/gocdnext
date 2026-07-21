import type { ReactNode } from "react";
import { ArrowLeftRight, CheckCircle2, Info } from "lucide-react";

import { cn } from "@/lib/utils";
import { analysisTone, imageParts, shortHash, TONE } from "@/lib/rollouts";
import type { Rollout } from "@/types/api";

// BlueGreenBlocks is the blue-green body (design handoff §8): the ACTIVE block (serving
// 100% of production), a swap arrow, and the PREVIEW block (serving 0% — preview only),
// with a note explaining the scale-down delay and what Reject does.
//
// Honesty note: the read API carries the two pod hashes (active=stable_hash,
// preview=pod_hash), the two service names and the PREVIEW image — but NOT the active
// revision's image or the per-revision commit lines the mock shows. So the active block
// is identified by its hash + service only; we never fabricate an active image/commit.
export function BlueGreenBlocks({ rollout }: { rollout: Rollout }) {
  const preview = imageParts(rollout.image);
  const analysis = rollout.analysis;
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-1 items-stretch gap-4 lg:grid-cols-[1fr_3rem_1fr]">
        <BgBlock role="active">
          <BgTag role="active">
            Active — {shortHash(rollout.stable_hash) || "current"}
          </BgTag>
          {/* No active image is exposed by the API — identify by hash + service only. */}
          <div className="font-mono text-[13px] text-muted-foreground">
            current active replica set
          </div>
          <dl className="mt-auto flex flex-col gap-2.5">
            <KV label="serving">
              <span className="font-semibold text-sky-600 dark:text-sky-400">
                100% production
              </span>
            </KV>
            <KV label="service">
              <ServiceValue service={rollout.active_service} />
            </KV>
          </dl>
        </BgBlock>

        <div
          className="flex items-center justify-center text-muted-foreground max-lg:rotate-90"
          aria-hidden
        >
          <div className="flex flex-col items-center gap-1.5">
            <ArrowLeftRight className="size-6" />
            <span className="max-w-[4rem] text-center font-mono text-[8.5px] uppercase leading-tight tracking-wide text-muted-foreground/80">
              promote swaps the active service
            </span>
          </div>
        </div>

        <BgBlock role="preview">
          <BgTag role="preview">
            Preview — {shortHash(rollout.pod_hash) || "new"}
          </BgTag>
          {preview.name ? (
            <div className="font-mono text-[13px] font-semibold text-foreground">
              {preview.name}
              {preview.tag ? (
                <span className="text-teal-600 dark:text-teal-400">
                  :{preview.tag}
                </span>
              ) : null}
            </div>
          ) : (
            <div className="font-mono text-[13px] text-muted-foreground">
              preview replica set
            </div>
          )}
          <dl className="mt-auto flex flex-col gap-2.5">
            <KV label="serving">
              <span className="text-muted-foreground">0% · preview only</span>
            </KV>
            <KV label="service">
              <ServiceValue service={rollout.preview_service} />
            </KV>
            {analysis ? (
              <KV label="pre-promotion">
                <span
                  className={cn(
                    "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] font-semibold",
                    TONE[analysisTone(analysis.phase)],
                  )}
                >
                  <CheckCircle2 className="size-3" aria-hidden />
                  {analysis.name || "analysis"}: {analysis.phase || "Unknown"}
                </span>
              </KV>
            ) : null}
          </dl>
        </BgBlock>
      </div>

      {analysis?.message ? (
        <p className="text-[11px] text-muted-foreground">{analysis.message}</p>
      ) : null}

      <Note rollout={rollout} />
    </div>
  );
}

function BgBlock({
  role,
  children,
}: {
  role: "active" | "preview";
  children: ReactNode;
}) {
  const active = role === "active";
  return (
    <div
      className={cn(
        "flex flex-col gap-3.5 rounded-xl border bg-gradient-to-b to-transparent p-5",
        active
          ? "border-sky-500/40 from-sky-500/10"
          : "border-emerald-500/40 from-emerald-500/10",
      )}
    >
      {children}
    </div>
  );
}

function BgTag({
  role,
  children,
}: {
  role: "active" | "preview";
  children: ReactNode;
}) {
  const active = role === "active";
  return (
    <span
      className={cn(
        "inline-flex items-center gap-2 self-start rounded-full border px-2.5 py-1 font-mono text-[10.5px] font-bold uppercase tracking-wide",
        active
          ? "border-sky-500/30 bg-sky-500/15 text-sky-600 dark:text-sky-400"
          : "border-emerald-500/40 bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
      )}
    >
      {children}
    </span>
  );
}

function KV({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3">
      <dt className="font-mono text-[11px] text-muted-foreground">{label}</dt>
      <dd className="min-w-0 truncate font-mono text-[12px]">{children}</dd>
    </div>
  );
}

// ServiceValue renders the k8s Service name in the teal accent. The API carries the
// service NAME but no reachable URL, so it is plain text — never a fabricated link.
function ServiceValue({ service }: { service: string }) {
  if (!service) {
    return <span className="text-muted-foreground">—</span>;
  }
  return (
    <span className="font-semibold text-teal-600 dark:text-teal-400">
      {service}
    </span>
  );
}

// Note explains the scale-down delay and what Reject does. The controller's default is
// 30s when scale_down_delay_seconds is unset (0); we surface the CR value as-is and note
// the default rather than synthesising it server-side.
function Note({ rollout }: { rollout: Rollout }) {
  const delay =
    rollout.scale_down_delay_seconds > 0
      ? `${rollout.scale_down_delay_seconds}s`
      : "30s (the controller default)";
  return (
    <p className="flex items-start gap-2 rounded-lg border border-border bg-muted/40 p-3 text-xs text-muted-foreground">
      <Info className="mt-0.5 size-4 shrink-0" aria-hidden />
      <span>
        <span className="font-medium text-foreground">Promote</span> points the{" "}
        <Kbd>active</Kbd> service at the preview revision. The old revision
        lingers for <Kbd>{delay}</Kbd> for an instant rollback, then scales
        down. <span className="font-medium text-foreground">Reject</span>{" "}
        discards the preview and keeps the current active revision — it does NOT
        revert Git.
      </span>
    </p>
  );
}

function Kbd({ children }: { children: ReactNode }) {
  return (
    <span className="rounded border border-teal-500/35 bg-teal-500/10 px-1.5 py-0.5 font-mono text-[11px] text-teal-600 dark:text-teal-400">
      {children}
    </span>
  );
}
