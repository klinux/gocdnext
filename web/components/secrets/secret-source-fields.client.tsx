"use client";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import type { SecretSource } from "@/types/api";

// SOURCE_LABELS is the single source of truth for the human label of
// each backend. The dialog selector, the table column, and the row
// summary all read from here so a new backend lands in one place.
export const SOURCE_LABELS: Record<SecretSource, string> = {
  db: "Stored value",
  vault: "HashiCorp Vault",
  gcp: "GCP Secret Manager",
  aws: "AWS Secrets Manager",
};

// SOURCE_ORDER pins the option order in the selector (db first, then
// the external backends) regardless of how the server lists them.
export const SOURCE_ORDER: SecretSource[] = ["db", "vault", "gcp", "aws"];

// vaultRequiresKey is the one backend-specific rule the UI enforces
// pre-flight: a Vault path addresses a map, so a key is mandatory.
export function vaultRequiresKey(source: SecretSource): boolean {
  return source === "vault";
}

// isSecretSource narrows an arbitrary string (e.g. an entry from the
// server's configured_sources list) to a known SecretSource so the
// selector never offers a backend the UI can't render fields for.
export function isSecretSource(s: string): s is SecretSource {
  return s === "db" || s === "vault" || s === "gcp" || s === "aws";
}

// gateSources resolves the selectable backends from the server's
// configured_sources list — the server is authoritative on what a write
// may pick. It reports "db" only when a cipher (GOCDNEXT_SECRET_KEY) is
// set, and each external backend only when enabled, so an external-only
// deployment never offers "db" (which the server would 503). The known
// sources are pinned to SOURCE_ORDER (db first) regardless of list order.
export function gateSources(configured: readonly string[]): SecretSource[] {
  const allowed = new Set<SecretSource>();
  for (const s of configured) {
    if (isSecretSource(s)) allowed.add(s);
  }
  return SOURCE_ORDER.filter((s) => allowed.has(s));
}

type Props = {
  source: SecretSource;
  sources: SecretSource[];
  onSourceChange: (s: SecretSource) => void;
  value: string;
  onValueChange: (v: string) => void;
  path: string;
  onPathChange: (v: string) => void;
  refKey: string;
  onRefKeyChange: (v: string) => void;
  // Rotate mode locks the source — you can't repoint an existing
  // secret at a different backend from the rotate flow (delete +
  // recreate to change the source).
  disabledSource?: boolean;
};

// SecretSourceFields owns the source selector plus the fieldset that
// swaps on the selected source: the value Textarea for "db", or the
// path/key inputs for an external backend. Split out of the dialog so
// the dialog file stays well under the ~400-line cap.
export function SecretSourceFields({
  source,
  sources,
  onSourceChange,
  value,
  onValueChange,
  path,
  onPathChange,
  refKey,
  onRefKeyChange,
  disabledSource,
}: Props) {
  const external = source !== "db";

  return (
    <>
      <div className="space-y-2">
        <Label htmlFor="secret-source">Source</Label>
        <Select
          items={SOURCE_LABELS}
          value={source}
          disabled={disabledSource}
          onValueChange={(next) => {
            // base-ui passes the value directly (null when cleared, which
            // this non-clearable select never emits). Narrow before
            // forwarding so an unexpected string can't slip through.
            if (typeof next === "string" && isSecretSource(next)) {
              onSourceChange(next);
            }
          }}
        >
          <SelectTrigger id="secret-source" aria-label="Source" className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {sources.map((s) => (
              <SelectItem key={s} value={s}>
                {SOURCE_LABELS[s]}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="text-[11px] text-muted-foreground">
          {external
            ? "The value stays in the external backend — gocdnext stores only the reference and resolves it at runtime."
            : "Encrypted at rest (AES-256-GCM) and never echoed back by the API."}
        </p>
      </div>

      {external ? (
        <>
          <div className="space-y-2">
            <Label htmlFor="secret-path">
              Path
              <span className="text-destructive"> *</span>
            </Label>
            <Input
              id="secret-path"
              name="path"
              autoComplete="off"
              spellCheck={false}
              value={path}
              onChange={(e) => onPathChange(e.target.value)}
              className="font-mono text-sm"
              placeholder={
                source === "vault"
                  ? "secret/data/ci/github"
                  : source === "gcp"
                    ? "gh-token"
                    : "ci/github-token"
              }
            />
            <p className="text-[11px] text-muted-foreground">
              Address of the secret in the backend.
            </p>
          </div>

          <div className="space-y-2">
            <Label htmlFor="secret-ref-key">
              Key
              {vaultRequiresKey(source) ? (
                <span className="text-destructive"> *</span>
              ) : (
                <span className="text-muted-foreground"> (optional)</span>
              )}
            </Label>
            <Input
              id="secret-ref-key"
              name="key"
              autoComplete="off"
              spellCheck={false}
              value={refKey}
              onChange={(e) => onRefKeyChange(e.target.value)}
              className="font-mono text-sm"
              placeholder="token"
            />
            <p className="text-[11px] text-muted-foreground">
              {vaultRequiresKey(source)
                ? "Required — a Vault path holds a map of keys."
                : "Leave blank when the path addresses a single value."}
            </p>
          </div>
        </>
      ) : (
        <div className="space-y-2">
          <Label htmlFor="secret-value">Value</Label>
          {/*
            break-all: wraps long single-line values (kubeconfigs,
            long base64 tokens) so they don't blow the dialog out
            of the viewport. wrap="soft": explicit so future
            shadcn updates can't flip it. max-h + overflow-y-auto:
            very long pastes scroll inside the textarea instead of
            pushing the footer buttons off-screen. maxLength:
            matches the server-side cap so the wire round-trip
            fails the same way locally.
          */}
          <Textarea
            id="secret-value"
            name="value"
            autoComplete="off"
            spellCheck={false}
            wrap="soft"
            maxLength={64 * 1024}
            rows={4}
            value={value}
            onChange={(e) => onValueChange(e.target.value)}
            className="w-full font-mono text-sm break-all resize-y max-h-[40vh] overflow-y-auto"
            placeholder="ghp_..."
          />
          <p className="text-[11px] text-muted-foreground">
            Multi-line values (PEM keys, etc.) are OK. Not visible after saving.
          </p>
        </div>
      )}
    </>
  );
}
