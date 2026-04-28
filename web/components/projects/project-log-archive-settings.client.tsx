"use client";

import { useState, useTransition } from "react";
import { Archive, Loader2 } from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { setProjectLogArchive } from "@/server/actions/project-settings";

type Mode = "inherit" | "on" | "off";

type Props = {
  slug: string;
  // initialEnabled mirrors the projects.log_archive_enabled column:
  //   null  -> inherit the global GOCDNEXT_LOG_ARCHIVE policy
  //   true  -> always archive (override)
  //   false -> never archive (override)
  initialEnabled: boolean | null;
  // globalPolicy is the operator-level default surfaced so the
  // "Inherit" option carries a hint of what that means right now.
  globalPolicy: "auto" | "on" | "off" | string;
  // hasArtifactBackend gates the live "currently archiving" hint —
  // even with the policy set to "on", archiving needs a backend.
  hasArtifactBackend: boolean;
};

function modeFromFlag(flag: boolean | null): Mode {
  if (flag === null) return "inherit";
  return flag ? "on" : "off";
}

function flagFromMode(mode: Mode): boolean | null {
  if (mode === "inherit") return null;
  return mode === "on";
}

// Resolves what would actually happen to a job given the current
// (mode, globalPolicy, hasArtifactBackend) — same EffectiveLogArchive
// rules as the server. Surfaced as a small "currently: archiving"
// hint so operators see the impact of the toggle without saving.
function resolvedHint(
  mode: Mode,
  globalPolicy: string,
  hasArtifactBackend: boolean,
): string {
  if (!hasArtifactBackend) {
    return "No artefact backend configured — archiving disabled cluster-wide.";
  }
  if (globalPolicy === "off") {
    return "Cluster policy is off; project overrides are ignored.";
  }
  const project = flagFromMode(mode);
  let willArchive: boolean;
  if (project === null) {
    willArchive = globalPolicy === "auto" || globalPolicy === "on";
  } else {
    willArchive = project;
  }
  return willArchive
    ? "Logs from terminal jobs will be archived to the artefact store and dropped from the database."
    : "Logs stay in the database and age out via retention only.";
}

export function ProjectLogArchiveSettings({
  slug,
  initialEnabled,
  globalPolicy,
  hasArtifactBackend,
}: Props) {
  const [mode, setMode] = useState<Mode>(modeFromFlag(initialEnabled));
  const [pending, startTransition] = useTransition();

  const dirty = mode !== modeFromFlag(initialEnabled);

  const onSave = () => {
    startTransition(async () => {
      const res = await setProjectLogArchive({
        slug,
        enabled: flagFromMode(mode),
      });
      if (res.ok) {
        toast.success(
          mode === "inherit"
            ? "Inheriting global policy"
            : mode === "on"
              ? "Always archiving logs"
              : "Never archiving logs",
        );
      } else {
        toast.error(res.error);
      }
    });
  };

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center gap-2">
          <Archive className="size-4 text-muted-foreground" aria-hidden />
          <CardTitle className="text-base">Log archive</CardTitle>
        </div>
        <CardDescription>
          When a job hits a terminal status its log lines are gzipped, shipped
          to the artefact store, and dropped from the database — keeping the
          partitioned heap lean. Cluster-wide policy is{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            {globalPolicy}
          </code>
          ; per-project overrides win except when the global is{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">off</code>.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div
          role="radiogroup"
          aria-label="Log archive policy"
          className="inline-flex flex-wrap gap-2"
        >
          {(["inherit", "on", "off"] as const).map((m) => (
            <ModeButton
              key={m}
              mode={m}
              active={mode === m}
              onClick={() => setMode(m)}
              disabled={pending}
            />
          ))}
        </div>

        <p className="text-xs text-muted-foreground">
          {resolvedHint(mode, globalPolicy, hasArtifactBackend)}
        </p>

        <Button onClick={onSave} disabled={!dirty || pending}>
          {pending ? (
            <Loader2 className="mr-2 size-4 animate-spin" aria-hidden />
          ) : null}
          Save
        </Button>
      </CardContent>
    </Card>
  );
}

function ModeButton({
  mode,
  active,
  onClick,
  disabled,
}: {
  mode: Mode;
  active: boolean;
  onClick: () => void;
  disabled: boolean;
}) {
  const labels: Record<Mode, string> = {
    inherit: "Use global default",
    on: "Always archive",
    off: "Never archive",
  };
  return (
    <button
      type="button"
      role="radio"
      aria-checked={active}
      onClick={onClick}
      disabled={disabled}
      className={cn(
        "rounded-md border px-3 py-1.5 text-sm transition-colors",
        active
          ? "border-primary bg-primary/10 text-primary"
          : "border-border bg-background hover:bg-muted",
        disabled && "pointer-events-none opacity-60",
      )}
    >
      {labels[mode]}
    </button>
  );
}
