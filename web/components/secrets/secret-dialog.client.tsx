"use client";

import { useState, useTransition, type ReactElement } from "react";
import { Plus, RotateCcw, AlertCircle, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { secretNameSchema, type SecretPayload } from "@/lib/validations";
import { setGlobalSecret, setSecret } from "@/server/actions/secrets";
import {
  SecretSourceFields,
  gateSources,
  vaultRequiresKey,
} from "@/components/secrets/secret-source-fields.client";
import type { SecretSource } from "@/types/api";

// Discriminated on scope so the caller can't accidentally pass a
// slug to a global secret or forget the slug on a project one. The
// shared `mode`/`name`/`trigger`/`configuredSources` props live in a
// base type so the two variants stay thin.
type SharedProps = {
  mode?: "create" | "rotate";
  name?: string;
  trigger?: ReactElement;
  // The sources the server reports writable on this deployment (db
  // present only when a cipher is set, plus each enabled external
  // backend). The selector is gated to exactly this list. Default
  // ["db"] keeps the dialog db-only when a caller doesn't pass it
  // (the common single-node case / standalone tests).
  configuredSources?: string[];
  // In rotate mode, the secret's current source + ref so the dialog opens
  // pre-filled — a Vault-backed secret opens as Vault with its path/key,
  // not reset to "Stored value" (which would silently repoint it to db on
  // submit). The value itself is never echoed back, so it stays empty.
  current?: { source: SecretSource; path?: string; key?: string };
};

type Props =
  | (SharedProps & { scope?: "project"; slug: string })
  | (SharedProps & { scope: "global" });

// buildPayload assembles the discriminated-union payload the set
// actions accept. Returns a string on a pre-flight failure so the
// error lands next to the form instead of after a server round-trip.
function buildPayload(
  source: SecretSource,
  name: string,
  value: string,
  path: string,
  refKey: string,
): SecretPayload | string {
  const nameCheck = secretNameSchema.safeParse(name);
  if (!nameCheck.success) {
    return nameCheck.error.issues[0]?.message ?? "invalid name";
  }
  if (source === "db") {
    if (value.length === 0) return "value cannot be empty";
    return { source: "db", name, value };
  }
  if (path.trim().length === 0) return "path is required";
  const key = refKey.trim();
  if (vaultRequiresKey(source) && key.length === 0) {
    return "key is required for Vault";
  }
  const ref = key.length > 0 ? { path, key } : { path };
  if (source === "vault") {
    // Narrowed above: vault always has a non-empty key here.
    return { source: "vault", name, ref: { path, key } };
  }
  return { source, name, ref };
}

// SecretDialog handles both creating a new secret (trigger = "New secret"
// button) and rotating an existing value (trigger = "Rotate" on a row).
// Rotate mode reuses the underlying upsert — the server endpoint is
// idempotent on (scope, name). The scope prop routes to the right
// server action.
export function SecretDialog(props: Props) {
  const isGlobal = props.scope === "global";
  const slug = isGlobal ? "" : props.slug;
  const {
    mode = "create",
    name = "",
    trigger,
    configuredSources = ["db"],
    current,
  } = props;
  const gated = gateSources(configuredSources);
  // Fall back to db only if the server somehow reported nothing writable —
  // the list endpoint 503s when neither cipher nor a backend is configured,
  // so in practice `gated` is always non-empty.
  const baseSources: SecretSource[] = gated.length > 0 ? gated : ["db"];
  // Rotate must keep the secret's own source selectable even if its backend
  // was since removed from the writable set — otherwise the Select would
  // render an value not in its options.
  const sources: SecretSource[] =
    current?.source && !baseSources.includes(current.source)
      ? [current.source, ...baseSources]
      : baseSources;
  // The initial source is the secret's current one (rotate) or the first
  // offered one (create) — so an external-only deployment opens on Vault.
  const initialSource: SecretSource = current?.source ?? sources[0] ?? "db";

  const [open, setOpen] = useState(false);
  const [formName, setFormName] = useState(name);
  const [source, setSource] = useState<SecretSource>(initialSource);
  const [value, setValue] = useState("");
  const [path, setPath] = useState(current?.path ?? "");
  const [refKey, setRefKey] = useState(current?.key ?? "");
  const [clientError, setClientError] = useState<string | null>(null);
  const [pending, startTransition] = useTransition();

  const rotating = mode === "rotate";

  function resetAndClose() {
    setFormName(name);
    setSource(initialSource);
    setValue("");
    setPath(current?.path ?? "");
    setRefKey(current?.key ?? "");
    setClientError(null);
    setOpen(false);
  }

  function onSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setClientError(null);

    const payload = buildPayload(source, formName, value, path, refKey);
    if (typeof payload === "string") {
      setClientError(payload);
      return;
    }

    startTransition(async () => {
      const res = isGlobal
        ? await setGlobalSecret(payload)
        : await setSecret({ slug, payload });
      if (!res.ok) {
        toast.error(`set secret: ${res.error}`);
        return;
      }
      const scopeLabel = isGlobal ? "Global secret" : "Secret";
      toast.success(
        res.created
          ? `${scopeLabel} ${formName} created`
          : `${scopeLabel} ${formName} updated`,
      );
      resetAndClose();
    });
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) resetAndClose();
        else setOpen(true);
      }}
    >
      <DialogTrigger render={
        trigger ??
          <Button size="sm">
            <Plus className="h-4 w-4" aria-hidden />
            New secret
          </Button>
      } />
      <DialogContent className="sm:max-w-lg max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>
            {rotating ? (
              <>
                Rotate <span className="font-mono">{name}</span>
              </>
            ) : (
              "New secret"
            )}
          </DialogTitle>
          <DialogDescription>
            {rotating
              ? "Set a new value (or reference) for this secret. The old one is replaced immediately."
              : "Store a value in-app or reference one from an external backend (Vault, GCP, AWS)."}
          </DialogDescription>
        </DialogHeader>

        <form onSubmit={onSubmit} className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="secret-name">Name</Label>
            <Input
              id="secret-name"
              name="name"
              autoComplete="off"
              spellCheck={false}
              readOnly={rotating}
              value={formName}
              onChange={(e) => setFormName(e.target.value)}
              className={cn("font-mono", rotating && "bg-muted")}
              placeholder="GH_TOKEN"
            />
            <p className="text-[11px] text-muted-foreground">
              Letters, digits, underscore. Must start with a letter.
            </p>
          </div>

          <SecretSourceFields
            source={source}
            sources={sources}
            onSourceChange={setSource}
            value={value}
            onValueChange={setValue}
            path={path}
            onPathChange={setPath}
            refKey={refKey}
            onRefKeyChange={setRefKey}
          />

          {clientError ? (
            <div
              role="alert"
              className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs text-destructive"
            >
              <AlertCircle className="mt-0.5 h-3.5 w-3.5" aria-hidden />
              <span>{clientError}</span>
            </div>
          ) : null}

          <DialogFooter>
            <DialogClose render={<Button variant="outline" type="button">Cancel</Button>} />
            <Button type="submit" disabled={pending}>
              {pending ? <Loader2 className="h-4 w-4 animate-spin" /> : rotating ? (
                <>
                  <RotateCcw className="h-4 w-4" aria-hidden />
                  Update value
                </>
              ) : (
                "Create"
              )}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
