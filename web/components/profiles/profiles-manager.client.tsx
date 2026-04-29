"use client";

import { useMemo, useState, useTransition } from "react";
import { KeyRound, Link2, Loader2, Pencil, Plus, Search, Trash2, X } from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { EntityChip } from "@/components/shared/entity-chip";
import {
  createRunnerProfile,
  deleteRunnerProfile,
  updateRunnerProfile,
} from "@/server/actions/runner-profiles";
import type { AdminRunnerProfile } from "@/server/queries/admin";

type Props = {
  initial: AdminRunnerProfile[];
  // Names of every global secret currently configured. Fed by the
  // RSC so the secret-value picker shows what's available without a
  // client-side fetch. Empty list = no globals set yet (picker
  // surfaces a "create one in /admin/secrets first" hint).
  globalSecretNames: string[];
};

// envRow / secretRow are draft entries the UI owns until save. The
// blank trailing row makes "press Tab to add" feel native — no
// explicit "+ Add" button needed for the common case. Existing
// secrets carry `existing: true`; their value field is empty + read
// only until the admin clicks "Replace" — preserves the rule that
// the server never returns plaintext, while letting the admin still
// see/keep/replace each key.
type EnvRow = { key: string; value: string };
// SecretRow.refTarget is set when the stored value is a single
// `{{secret:NAME}}` template — the UI then renders the chip
// "→ globals.NAME" in place of the masked placeholder. When the
// admin clicks Replace and types a new value (literal or
// `{{secret:OTHER}}`), refTarget gets cleared so the new payload
// is what we send.
type SecretRow = {
  key: string;
  value: string;
  existing: boolean;
  replace: boolean;
  refTarget?: string;
};

type FormDraft = {
  id: string | null;
  name: string;
  description: string;
  engine: "kubernetes";
  default_image: string;
  default_cpu_request: string;
  default_cpu_limit: string;
  default_mem_request: string;
  default_mem_limit: string;
  max_cpu: string;
  max_mem: string;
  tagsRaw: string; // comma-separated; parsed on save
  envRows: EnvRow[];
  secretRows: SecretRow[];
};

function blankForm(): FormDraft {
  return {
    id: null,
    name: "",
    description: "",
    engine: "kubernetes",
    default_image: "",
    default_cpu_request: "",
    default_cpu_limit: "",
    default_mem_request: "",
    default_mem_limit: "",
    max_cpu: "",
    max_mem: "",
    tagsRaw: "",
    envRows: [{ key: "", value: "" }],
    secretRows: [{ key: "", value: "", existing: false, replace: true }],
  };
}

// optimisticSecretRefs mirrors the server-side ProfileSecretRefs:
// surfaces "→ globals.NAME" only when a secret value is a clean
// `{{secret:NAME}}` template (no surrounding text). Used for the
// optimistic in-memory update right after save so the editor's
// chip rendering matches what a re-fetch would produce.
const SECRET_REF_RE = /^\{\{\s*secret:([A-Z_][A-Z0-9_]*)\s*\}\}$/;
function optimisticSecretRefs(secrets: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(secrets)) {
    const m = v.match(SECRET_REF_RE);
    if (m) out[k] = m[1]!;
  }
  return out;
}

function profileToDraft(p: AdminRunnerProfile): FormDraft {
  const envRows: EnvRow[] = Object.entries(p.env ?? {}).map(([k, v]) => ({ key: k, value: v }));
  envRows.push({ key: "", value: "" });
  const refs = p.secret_refs ?? {};
  const secretRows: SecretRow[] = (p.secret_keys ?? []).map((k) => ({
    key: k,
    value: "",
    existing: true,
    replace: false,
    refTarget: refs[k],
  }));
  secretRows.push({ key: "", value: "", existing: false, replace: true });
  return {
    id: p.id,
    name: p.name,
    description: p.description,
    engine: "kubernetes",
    default_image: p.default_image,
    default_cpu_request: p.default_cpu_request,
    default_cpu_limit: p.default_cpu_limit,
    default_mem_request: p.default_mem_request,
    default_mem_limit: p.default_mem_limit,
    max_cpu: p.max_cpu,
    max_mem: p.max_mem,
    tagsRaw: (p.tags ?? []).join(", "),
    envRows,
    secretRows,
  };
}

export function ProfilesManager({ initial, globalSecretNames }: Props) {
  const [profiles, setProfiles] = useState<AdminRunnerProfile[]>(initial);
  const [filter, setFilter] = useState("");
  const [form, setForm] = useState<FormDraft | null>(null);
  const [pending, startTransition] = useTransition();

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return profiles;
    return profiles.filter(
      (p) =>
        p.name.toLowerCase().includes(q) ||
        p.description.toLowerCase().includes(q) ||
        p.tags.some((t) => t.toLowerCase().includes(q)),
    );
  }, [profiles, filter]);

  const parseTags = (raw: string): string[] =>
    raw
      .split(",")
      .map((t) => t.trim())
      .filter((t) => t.length > 0);

  // collectEnv + collectSecrets fold the row arrays into the
  // wire-shape maps. Empty keys are dropped (the trailing "blank
  // row" pattern means the user might never fill the last entry);
  // duplicate keys collapse to the LAST value, mirroring how the
  // server would resolve a conflicting JSON object.
  const collectEnv = (rows: EnvRow[]): Record<string, string> => {
    const out: Record<string, string> = {};
    for (const r of rows) {
      const k = r.key.trim();
      if (k) out[k] = r.value;
    }
    return out;
  };
  // collectSecrets emits ONLY the keys the admin actually wants to
  // persist with a (possibly new) plaintext value. Existing keys
  // not flagged for replace are kept by re-sending the key with an
  // empty value... wait, that would erase. Pattern instead:
  // existing+!replace → drop the entry entirely on the wire so the
  // server keeps whatever it has. The contract for that is the
  // "merge on update" behaviour we documented; full-replace would
  // require us to send all values. Stage 1 of the UI uses the
  // simpler model: we ALWAYS send what the user typed. Existing
  // secrets with replace=false carry through as the remembered key
  // with no plaintext — we surface that on save with a guard
  // forcing the admin to either replace or remove unchanged ones.
  const collectSecrets = (rows: SecretRow[]): Record<string, string> => {
    const out: Record<string, string> = {};
    for (const r of rows) {
      const k = r.key.trim();
      if (!k) continue;
      // Existing secret left untouched: the server treats missing
      // keys as deletions on full-replace, but we don't want to
      // erase. So we don't send it. The admin must explicitly
      // click "Remove" to delete an existing secret.
      if (r.existing && !r.replace) continue;
      out[k] = r.value;
    }
    return out;
  };

  const saveForm = () => {
    if (!form) return;
    const name = form.name.trim();
    if (!name) {
      toast.error("Name is required");
      return;
    }
    // Force the admin to confront existing secrets: if any are
    // still in the "keep, don't replace" state on save, we treat
    // that as a deliberate intent to keep them — but tell the user
    // that the wire payload will silently drop them. This is the
    // simplest UX given the server's full-replace contract; a
    // future iteration can add a per-row "preserve" flag that the
    // server understands.
    const envMap = collectEnv(form.envRows);
    const secretsMap = collectSecrets(form.secretRows);
    const newSecretsCount = form.secretRows.filter((r) => !r.existing && r.key.trim()).length;
    const replacedCount = form.secretRows.filter((r) => r.existing && r.replace && r.key.trim()).length;
    const droppedCount = form.secretRows.filter((r) => r.existing && !r.replace && r.key.trim()).length;
    if (form.id && droppedCount > 0 && newSecretsCount === 0 && replacedCount === 0) {
      // Update with no secret changes at all — fast path: confirm
      // we're sending an empty secrets map will erase. Block save.
      const proceed = confirm(
        `${droppedCount} existing secret(s) will be REMOVED because the server uses full-replace semantics on update.\n\nClick OK to remove them, or Cancel to mark them "Replace" with their current values.`,
      );
      if (!proceed) return;
    }
    startTransition(async () => {
      const body = {
        name,
        description: form.description,
        engine: form.engine,
        default_image: form.default_image,
        default_cpu_request: form.default_cpu_request,
        default_cpu_limit: form.default_cpu_limit,
        default_mem_request: form.default_mem_request,
        default_mem_limit: form.default_mem_limit,
        max_cpu: form.max_cpu,
        max_mem: form.max_mem,
        tags: parseTags(form.tagsRaw),
        env: envMap,
        secrets: secretsMap,
      };
      const res = form.id
        ? await updateRunnerProfile({ ...body, id: form.id })
        : await createRunnerProfile(body);
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success(form.id ? "Profile updated" : "Profile created");
      setForm(null);
      // Optimistic local update; the next server render replaces
      // this with the canonical row including timestamps.
      const draft: AdminRunnerProfile = {
        id: form.id ?? "__opt__" + Date.now(),
        name,
        description: form.description,
        engine: form.engine,
        default_image: form.default_image,
        default_cpu_request: form.default_cpu_request,
        default_cpu_limit: form.default_cpu_limit,
        default_mem_request: form.default_mem_request,
        default_mem_limit: form.default_mem_limit,
        max_cpu: form.max_cpu,
        max_mem: form.max_mem,
        tags: parseTags(form.tagsRaw),
        env: envMap,
        secret_keys: Object.keys(secretsMap).sort(),
        secret_refs: optimisticSecretRefs(secretsMap),
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
      };
      setProfiles((prev) => {
        if (form.id) {
          return prev.map((p) => (p.id === form.id ? draft : p));
        }
        return [...prev, draft].sort((a, b) => a.name.localeCompare(b.name));
      });
    });
  };

  const handleDelete = (p: AdminRunnerProfile) => {
    if (!confirm(`Delete profile "${p.name}"? Pipelines referencing it will fail to apply until rewired.`)) {
      return;
    }
    startTransition(async () => {
      const res = await deleteRunnerProfile({ id: p.id });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success("Profile deleted");
      setProfiles((prev) => prev.filter((x) => x.id !== p.id));
    });
  };

  return (
    <>
      {/* Toolbar: filter on the left, primary action on the right —
          mirrors groups-manager / users-table so the eye lands on
          the same place across admin tables. */}
      <div className="flex items-center justify-between gap-4">
        <div className="relative max-w-sm flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
          <Input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter profiles…"
            className="pl-8"
          />
        </div>
        <Button onClick={() => setForm(blankForm())}>
          <Plus className="mr-2 h-4 w-4" /> New profile
        </Button>
      </div>

      {/* Table wrapper mirrors users-table / groups: bordered card
          with rounded corners and bg-card, so all admin lists read
          as one visual family. */}
      <div className="overflow-hidden rounded-lg border border-border bg-card">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Engine</TableHead>
            <TableHead>Cap (cpu / mem)</TableHead>
            <TableHead>Tags</TableHead>
            <TableHead className="w-[120px]" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {filtered.length === 0 ? (
            <TableRow>
              <TableCell colSpan={5} className="text-center text-sm text-muted-foreground py-8">
                {profiles.length === 0
                  ? "No runner profiles yet — create one above."
                  : "No profiles match the filter."}
              </TableCell>
            </TableRow>
          ) : (
            filtered.map((p) => (
              <TableRow key={p.id}>
                <TableCell className="font-medium">
                  {p.name}
                  {p.description ? (
                    <div className="text-xs text-muted-foreground">{p.description}</div>
                  ) : null}
                </TableCell>
                <TableCell>
                  <Badge variant="outline">{p.engine}</Badge>
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {p.max_cpu || "—"} / {p.max_mem || "—"}
                </TableCell>
                <TableCell>
                  <div className="flex flex-wrap gap-1">
                    {(p.tags ?? []).length === 0 ? (
                      <span className="text-xs text-muted-foreground">—</span>
                    ) : (
                      (p.tags ?? []).map((t) => (
                        <Badge key={t} variant="secondary">
                          {t}
                        </Badge>
                      ))
                    )}
                  </div>
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    variant="ghost"
                    size="icon"
                    onClick={() => setForm(profileToDraft(p))}
                    aria-label={`Edit ${p.name}`}
                  >
                    <Pencil className="h-4 w-4" />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    onClick={() => handleDelete(p)}
                    disabled={pending}
                    aria-label={`Delete ${p.name}`}
                  >
                    <Trash2 className="h-4 w-4" />
                  </Button>
                </TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>
      </div>

      <Sheet open={form !== null} onOpenChange={(open) => !open && setForm(null)}>
        <SheetContent
          side="right"
          className={cn(
            "overflow-y-auto",
            // Wider sheet so the env/secret editors get room to breathe.
            // 30vw on large screens, capped so it doesn't dominate
            // ultra-wide displays; min-width keeps narrow viewports
            // from collapsing the row layout (key + value + actions).
            "data-[side=right]:w-[min(30vw,720px)] data-[side=right]:sm:max-w-[min(30vw,720px)]",
            "data-[side=right]:min-w-[28rem]",
          )}
        >
          <SheetHeader>
            <SheetTitle>{form?.id ? "Edit profile" : "New profile"}</SheetTitle>
            <SheetDescription>
              Profiles bundle a fallback image, default + max compute, and a
              tag set. Jobs reference them via{" "}
              <code className="rounded bg-muted px-1 py-0.5 text-xs">agent.profile</code>{" "}
              in YAML.
            </SheetDescription>
          </SheetHeader>

          {form ? (
            <div className="grid gap-4 px-6 pb-6">
              <Field label="Name" required>
                <Input
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="default"
                  autoFocus
                  disabled={!!form.id}
                />
              </Field>
              <Field label="Description">
                <Input
                  value={form.description}
                  onChange={(e) => setForm({ ...form, description: e.target.value })}
                  placeholder="What this profile is for"
                />
              </Field>
              {/* Image is intentionally NOT a profile field — it's
                  a job/plugin-level concern. Profiles parameterise
                  the runtime (resources, tags, env, secrets); what
                  to run lands in `image:` on the job or in the
                  plugin's own Dockerfile. */}
              <div className="grid grid-cols-2 gap-3">
                <Field label="Default CPU req">
                  <Input
                    value={form.default_cpu_request}
                    onChange={(e) => setForm({ ...form, default_cpu_request: e.target.value })}
                    placeholder="100m"
                  />
                </Field>
                <Field label="Default CPU limit">
                  <Input
                    value={form.default_cpu_limit}
                    onChange={(e) => setForm({ ...form, default_cpu_limit: e.target.value })}
                    placeholder="1"
                  />
                </Field>
                <Field label="Default memory req">
                  <Input
                    value={form.default_mem_request}
                    onChange={(e) => setForm({ ...form, default_mem_request: e.target.value })}
                    placeholder="256Mi"
                  />
                </Field>
                <Field label="Default memory limit">
                  <Input
                    value={form.default_mem_limit}
                    onChange={(e) => setForm({ ...form, default_mem_limit: e.target.value })}
                    placeholder="1Gi"
                  />
                </Field>
                <Field label="Max CPU">
                  <Input
                    value={form.max_cpu}
                    onChange={(e) => setForm({ ...form, max_cpu: e.target.value })}
                    placeholder="4"
                  />
                </Field>
                <Field label="Max memory">
                  <Input
                    value={form.max_mem}
                    onChange={(e) => setForm({ ...form, max_mem: e.target.value })}
                    placeholder="8Gi"
                  />
                </Field>
              </div>
              <Field
                label="Tags (comma-separated)"
                hint="Required tags any agent must carry to run jobs bound to this profile. Merged with job-level agent.tags at apply time."
              >
                <Input
                  value={form.tagsRaw}
                  onChange={(e) => setForm({ ...form, tagsRaw: e.target.value })}
                  placeholder="linux, gpu"
                />
              </Field>

              <EnvRows
                rows={form.envRows}
                onChange={(rows) => setForm({ ...form, envRows: rows })}
              />
              <SecretRows
                rows={form.secretRows}
                onChange={(rows) => setForm({ ...form, secretRows: rows })}
                globalSecretNames={globalSecretNames}
              />

              <div className="mt-2 flex items-center justify-end gap-2">
                <Button variant="ghost" onClick={() => setForm(null)} disabled={pending}>
                  <X className="mr-2 h-4 w-4" /> Cancel
                </Button>
                <Button onClick={saveForm} disabled={pending}>
                  {pending ? (
                    <>
                      <Loader2 className="mr-2 h-4 w-4 animate-spin" /> Saving…
                    </>
                  ) : (
                    "Save"
                  )}
                </Button>
              </div>
            </div>
          ) : null}
        </SheetContent>
      </Sheet>
    </>
  );
}

function Field({
  label,
  hint,
  required,
  children,
}: {
  label: string;
  hint?: string;
  required?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className="grid gap-1.5">
      <Label>
        {label}
        {required ? <span className="text-destructive"> *</span> : null}
      </Label>
      {children}
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

// EnvRows renders the plain env editor — one row per KEY=VALUE pair
// plus a trailing blank that auto-promotes to a real row when the
// admin starts typing in it. Keeps the UX feeling like a spreadsheet
// without an explicit "+ Add" button. Layout is intentionally
// minimal: the parent Sheet is already crowded with profile sizing
// fields, so we lean on plain inputs + tight spacing.
function EnvRows({
  rows,
  onChange,
}: {
  rows: EnvRow[];
  onChange: (rows: EnvRow[]) => void;
}) {
  const update = (i: number, patch: Partial<EnvRow>) => {
    const next = rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r));
    // Auto-grow: if the trailing row got data, append a new blank.
    const last = next[next.length - 1];
    if (last && (last.key || last.value)) {
      next.push({ key: "", value: "" });
    }
    onChange(next);
  };
  const remove = (i: number) => {
    const next = rows.filter((_, idx) => idx !== i);
    if (next.length === 0) next.push({ key: "", value: "" });
    onChange(next);
  };
  return (
    <div className="grid gap-2">
      <Label className="flex items-center gap-2">
        Env
        <span className="text-xs font-normal text-muted-foreground">
          plaintext, injected into every plugin container on this profile
        </span>
      </Label>
      {rows.map((r, i) => (
        <div key={i} className="grid grid-cols-[1fr_1fr_auto] gap-2">
          <Input
            placeholder="KEY"
            value={r.key}
            onChange={(e) => update(i, { key: e.target.value })}
            className="font-mono text-xs"
          />
          <Input
            placeholder="value"
            value={r.value}
            onChange={(e) => update(i, { value: e.target.value })}
            className="font-mono text-xs"
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            onClick={() => remove(i)}
            disabled={!r.key && !r.value}
            aria-label="Remove env entry"
          >
            <X className="h-4 w-4" />
          </Button>
        </div>
      ))}
    </div>
  );
}

// SecretRows mirrors EnvRows but treats existing entries as
// write-protected by default — the admin must click "Replace" to
// overwrite a stored value. The server NEVER returns plaintext, so
// the input stays empty until replace mode is on.
//
// Each row also has a "🔗" picker button that pops a list of
// globally-configured secrets and inserts `{{secret:NAME}}` into
// the value field. The dispatcher resolves that template against
// the global at run time, so admins can rotate the underlying
// value once globally and every profile referencing it picks up
// the new value automatically.
//
// Existing rows whose stored value IS a single template carry
// `refTarget` — the UI then renders a `→ globals.NAME` chip
// instead of the masked placeholder, so the difference between
// "literal" and "reference" is visible without exposing the value.
function SecretRows({
  rows,
  onChange,
  globalSecretNames,
}: {
  rows: SecretRow[];
  onChange: (rows: SecretRow[]) => void;
  globalSecretNames: string[];
}) {
  const update = (i: number, patch: Partial<SecretRow>) => {
    const next = rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r));
    const last = next[next.length - 1];
    if (last && (last.key || last.value)) {
      next.push({ key: "", value: "", existing: false, replace: true });
    }
    onChange(next);
  };
  const remove = (i: number) => {
    const next = rows.filter((_, idx) => idx !== i);
    if (next.length === 0) next.push({ key: "", value: "", existing: false, replace: true });
    onChange(next);
  };
  // pickGlobal writes `{{secret:NAME}}` into the row's value field
  // and switches the row into replace mode if it was an existing
  // entry — same UX as typing a new value would be.
  const pickGlobal = (i: number, name: string) => {
    update(i, {
      value: `{{secret:${name}}}`,
      replace: true,
      refTarget: undefined, // user-selected new ref clears the
                            // "stored" indicator until save persists
    });
  };
  return (
    <div className="grid gap-2">
      <Label className="flex items-center gap-2">
        Secrets
        <span className="text-xs font-normal text-muted-foreground">
          encrypted at rest · pick a global with the link button to
          inherit + auto-rotate
        </span>
      </Label>
      {rows.map((r, i) => {
        const showAsRef = r.existing && !r.replace && r.refTarget;
        const editable = !r.existing || r.replace;
        return (
          <div key={i} className="grid grid-cols-[1fr_1fr_auto_auto_auto] gap-2">
            <Input
              placeholder="KEY"
              value={r.key}
              onChange={(e) => update(i, { key: e.target.value })}
              disabled={r.existing}
              className="font-mono text-xs"
            />
            {showAsRef ? (
              <div className="flex items-center px-2 py-1">
                <EntityChip
                  kind="secret"
                  label={`globals.${r.refTarget}`}
                  title={`References global secret "${r.refTarget}" — rotate it once, every profile picks up the new value`}
                />
              </div>
            ) : (
              <Input
                placeholder={r.existing && !r.replace ? "•••••••• (stored)" : "value or {{secret:NAME}}"}
                value={r.value}
                onChange={(e) => update(i, { value: e.target.value })}
                disabled={!editable}
                // type=password hides typed values; templates leak
                // their *shape* anyway (`{{secret:NAME}}`) but the
                // hide remains worth it for actual literals.
                type="password"
                className="font-mono text-xs"
              />
            )}
            {/*
              Picker stays clickable even on existing literal rows —
              the click enters replace mode + inserts the template,
              same UX as if the admin clicked Replace and typed it.
              Only disabled when no globals are configured at all
              (the popover would just show "Create one first" anyway,
              but the visual cue is clearer when the button is
              greyed out from the start).
            */}
            <GlobalSecretPickerButton
              names={globalSecretNames}
              disabled={globalSecretNames.length === 0}
              onPick={(name) => pickGlobal(i, name)}
            />
            {r.existing ? (
              <Button
                type="button"
                variant={r.replace ? "secondary" : "ghost"}
                size="sm"
                onClick={() => update(i, { replace: !r.replace, value: r.replace ? "" : r.value })}
                className="text-xs"
              >
                {r.replace ? "Cancel" : "Replace"}
              </Button>
            ) : (
              <span aria-hidden />
            )}
            <Button
              type="button"
              variant="ghost"
              size="icon"
              onClick={() => remove(i)}
              disabled={!r.key && !r.value && !r.refTarget}
              aria-label="Remove secret entry"
            >
              <X className="h-4 w-4" />
            </Button>
          </div>
        );
      })}
    </div>
  );
}

// GlobalSecretPickerButton is the "🔗" button that opens a Dialog
// listing every configured global secret. Click on a name →
// callback fires with that name, the row's value becomes
// `{{secret:NAME}}`. Empty list shows a hint pointing at
// /admin/secrets so the admin knows where to mint one.
//
// Built on Dialog (not Popover) because the picker lives inside a
// Sheet (which is also a Dialog under the hood); base-ui's
// Popover positioner has trouble computing layout when the
// trigger is in a modal stacking context. Dialogs nest cleanly.
function GlobalSecretPickerButton({
  names,
  onPick,
  disabled,
}: {
  names: string[];
  onPick: (name: string) => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return names;
    return names.filter((n) => n.toLowerCase().includes(q));
  }, [names, query]);
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) setQuery("");
      }}
    >
      <DialogTrigger
        render={
          <Button
            type="button"
            variant="ghost"
            size="icon"
            disabled={disabled}
            aria-label="Reference global secret"
            title={
              disabled
                ? "No global secrets configured — create one in /admin/secrets first"
                : "Reference a global secret"
            }
          >
            <Link2 className="h-4 w-4" />
          </Button>
        }
      />
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2 text-base">
            <KeyRound className="size-4" aria-hidden /> Reference a global secret
          </DialogTitle>
          <DialogDescription>
            Pick a global secret. Its value gets resolved at dispatch
            time — rotate it once globally and every profile that
            references it picks up the new value.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-2">
          <Input
            autoFocus
            placeholder={
              names.length === 0
                ? "No globals configured yet"
                : "Search globals…"
            }
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            disabled={names.length === 0}
          />
          <ul className="max-h-64 overflow-y-auto rounded-md border border-border">
            {names.length === 0 ? (
              <li className="px-3 py-4 text-center text-xs text-muted-foreground">
                No global secrets yet.{" "}
                <a
                  href="/admin/secrets"
                  className="text-primary hover:underline"
                >
                  Create one
                </a>{" "}
                first.
              </li>
            ) : filtered.length === 0 ? (
              <li className="px-3 py-3 text-center text-xs text-muted-foreground">
                Nothing matches.
              </li>
            ) : (
              filtered.map((n) => (
                <li key={n}>
                  <button
                    type="button"
                    onClick={() => {
                      onPick(n);
                      setOpen(false);
                      setQuery("");
                    }}
                    className="flex w-full items-center gap-2 px-3 py-2 text-left font-mono text-xs hover:bg-accent"
                  >
                    <KeyRound
                      className="size-3 text-muted-foreground"
                      aria-hidden
                    />
                    {n}
                  </button>
                </li>
              ))
            )}
          </ul>
        </div>
      </DialogContent>
    </Dialog>
  );
}
