import { KeyRound } from "lucide-react";

import { Label } from "@/components/ui/label";

// Shared form atoms used by the storage panels. Kept presentational
// (no client hooks) so the panels can compose them freely without
// dragging another "use client" boundary.

export function Field({
  label,
  hint,
  required,
  className,
  children,
}: {
  label: string;
  hint?: string;
  required?: boolean;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={className}>
      <Label className="text-xs">
        {label}
        {required ? (
          <span className="ml-1 text-destructive" aria-hidden>
            *
          </span>
        ) : null}
      </Label>
      <div className="mt-1">{children}</div>
      {hint ? (
        <p className="mt-1 text-[11px] text-muted-foreground">{hint}</p>
      ) : null}
    </div>
  );
}

export function BoolRow({
  label,
  hint,
  checked,
  onChange,
}: {
  label: string;
  hint?: string;
  checked: boolean;
  onChange: (v: boolean) => void;
}) {
  return (
    <label className="flex items-start gap-3 rounded-md border bg-background p-3 text-sm">
      <input
        type="checkbox"
        className="mt-0.5"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span className="flex-1">
        <span className="font-medium">{label}</span>
        {hint ? (
          <span className="block text-xs text-muted-foreground">{hint}</span>
        ) : null}
      </span>
    </label>
  );
}

export function CredentialsHeader({
  configured,
  replace,
  onReplaceChange,
  emptyHint,
}: {
  configured: boolean;
  replace: boolean;
  onReplaceChange: (v: boolean) => void;
  emptyHint: string;
}) {
  return (
    <div className="flex items-center justify-between gap-3 border-t pt-4">
      <div className="space-y-0.5">
        <div className="flex items-center gap-2 text-sm font-medium">
          <KeyRound className="size-4 text-muted-foreground" aria-hidden />
          Credentials
        </div>
        <p className="text-xs text-muted-foreground">
          {configured
            ? replace
              ? "Replacing stored credentials — saving will overwrite the secret."
              : "Stored encrypted at rest. Toggle to replace."
            : emptyHint}
        </p>
      </div>
      {configured ? (
        <label className="flex items-center gap-2 text-xs">
          <input
            type="checkbox"
            checked={replace}
            onChange={(e) => onReplaceChange(e.target.checked)}
          />
          Replace
        </label>
      ) : null}
    </div>
  );
}

export function FilesystemPanel() {
  return (
    <div className="rounded-md border bg-muted/30 p-4 text-sm">
      <p>
        Filesystem backend stores artifacts on a PVC mounted at{" "}
        <code className="rounded bg-muted px-1 font-mono text-xs">
          GOCDNEXT_ARTIFACTS_FS_ROOT
        </code>
        . The path is process-level and not configurable here — switching it
        at runtime would orphan existing artifacts.
      </p>
      <p className="mt-2 text-muted-foreground">
        Saving this option clears the DB override of any S3/GCS settings
        previously set; the server then falls back to its env-configured
        filesystem root.
      </p>
    </div>
  );
}
