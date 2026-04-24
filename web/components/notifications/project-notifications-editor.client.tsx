"use client";

import { useState, useTransition } from "react";
import { AlertCircle, Loader2, Save, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { setProjectNotifications } from "@/server/actions/project-notifications";
import type { ProjectNotification } from "@/server/queries/projects";

type Props = {
  slug: string;
  initial: ProjectNotification[];
};

// Internal editor shape — same as ProjectNotification but `with`
// is kept as raw YAML text so the user can paste an arbitrary
// set of keys without fighting a per-key form. Parsed into a
// Record<string,string> right before saving.
type DraftEntry = {
  on: ProjectNotification["on"];
  uses: string;
  withYaml: string;
  secrets: string;
};

const TRIGGERS: ProjectNotification["on"][] = [
  "failure",
  "success",
  "always",
  "canceled",
];

// ProjectNotificationsEditor renders the project-level list as a
// stack of cards (one per entry) plus add/remove buttons. Save
// hits the PUT endpoint with the full list — the API replaces
// atomically, so a failed save doesn't leave half the list
// updated. `with:` is edited as `KEY: VALUE` lines per entry
// rather than a nested form — faster to author, matches the YAML
// the user would write by hand.
export function ProjectNotificationsEditor({ slug, initial }: Props) {
  const [entries, setEntries] = useState<DraftEntry[]>(() =>
    initial.map(toDraft),
  );
  const [error, setError] = useState<string | null>(null);
  const [pending, startTransition] = useTransition();

  const addEntry = () => {
    setEntries((prev) => [
      ...prev,
      { on: "failure", uses: "", withYaml: "", secrets: "" },
    ]);
  };

  const removeEntry = (idx: number) => {
    setEntries((prev) => prev.filter((_, i) => i !== idx));
  };

  const updateEntry = (idx: number, patch: Partial<DraftEntry>) => {
    setEntries((prev) =>
      prev.map((e, i) => (i === idx ? { ...e, ...patch } : e)),
    );
  };

  const save = () => {
    setError(null);
    let payload: ProjectNotification[];
    try {
      payload = entries.map(fromDraft);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      return;
    }
    startTransition(async () => {
      const result = await setProjectNotifications({
        slug,
        notifications: payload,
      });
      if (!result.ok) {
        setError(result.error);
        toast.error("Save failed", { description: result.error });
        return;
      }
      toast.success(
        payload.length === 0
          ? "Cleared project notifications"
          : `Saved ${payload.length} notification${payload.length === 1 ? "" : "s"}`,
      );
    });
  };

  return (
    <div className="space-y-4">
      {entries.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border p-8 text-center text-sm text-muted-foreground">
          No project-level notifications. Pipelines under this project run
          their own <code className="rounded bg-muted px-1 py-0.5 text-xs">notifications:</code>{" "}
          block or nothing at all.
        </div>
      ) : (
        <div className="space-y-3">
          {entries.map((e, i) => (
            <EntryCard
              key={i}
              idx={i}
              entry={e}
              onChange={(patch) => updateEntry(i, patch)}
              onRemove={() => removeEntry(i)}
            />
          ))}
        </div>
      )}

      {error ? (
        <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
          <AlertCircle className="mt-0.5 size-4 shrink-0" aria-hidden />
          <span className="whitespace-pre-wrap break-words">{error}</span>
        </div>
      ) : null}

      <div className="flex items-center justify-between gap-2">
        <Button variant="outline" onClick={addEntry} disabled={pending}>
          Add notification
        </Button>
        <Button onClick={save} disabled={pending}>
          {pending ? (
            <Loader2 className="mr-1.5 size-4 animate-spin" aria-hidden />
          ) : (
            <Save className="mr-1.5 size-4" aria-hidden />
          )}
          Save
        </Button>
      </div>
    </div>
  );
}

function EntryCard({
  idx,
  entry,
  onChange,
  onRemove,
}: {
  idx: number;
  entry: DraftEntry;
  onChange: (patch: Partial<DraftEntry>) => void;
  onRemove: () => void;
}) {
  return (
    <div className="rounded-lg border border-border bg-card p-4">
      <div className="mb-3 flex items-center justify-between gap-2">
        <span className="text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
          #{idx + 1}
        </span>
        <Button
          variant="ghost"
          size="sm"
          onClick={onRemove}
          className="text-muted-foreground hover:text-destructive"
          aria-label={`Remove notification ${idx + 1}`}
        >
          <Trash2 className="size-4" aria-hidden />
        </Button>
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-[140px_1fr]">
        <div className="space-y-1.5">
          <Label htmlFor={`on-${idx}`} className="text-xs">
            On
          </Label>
          {/* Native <select> keeps this screen dependency-free
              (no shadcn Select component installed here); the
              closed set of 4 triggers doesn't need search/combo
              UX. */}
          <select
            id={`on-${idx}`}
            value={entry.on}
            onChange={(ev) =>
              onChange({ on: ev.target.value as DraftEntry["on"] })
            }
            className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-ring"
          >
            {TRIGGERS.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor={`uses-${idx}`} className="text-xs">
            Uses
          </Label>
          <Input
            id={`uses-${idx}`}
            value={entry.uses}
            onChange={(ev) => onChange({ uses: ev.target.value })}
            placeholder="gocdnext/slack@v1"
            className="font-mono"
          />
        </div>
      </div>

      <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-2">
        <div className="space-y-1.5">
          <Label htmlFor={`with-${idx}`} className="text-xs">
            With (KEY: value per line)
          </Label>
          <Textarea
            id={`with-${idx}`}
            value={entry.withYaml}
            onChange={(ev) => onChange({ withYaml: ev.target.value })}
            placeholder={"webhook: ${{ SLACK_WEBHOOK }}\nchannel: '#eng'"}
            rows={4}
            className={cn("font-mono text-xs")}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor={`secrets-${idx}`} className="text-xs">
            Secrets (one name per line)
          </Label>
          <Textarea
            id={`secrets-${idx}`}
            value={entry.secrets}
            onChange={(ev) => onChange({ secrets: ev.target.value })}
            placeholder="SLACK_WEBHOOK"
            rows={4}
            className="font-mono text-xs"
          />
        </div>
      </div>
    </div>
  );
}

function toDraft(n: ProjectNotification): DraftEntry {
  return {
    on: n.on,
    uses: n.uses,
    withYaml: n.with
      ? Object.entries(n.with)
          .map(([k, v]) => `${k}: ${v}`)
          .join("\n")
      : "",
    secrets: (n.secrets ?? []).join("\n"),
  };
}

function fromDraft(d: DraftEntry): ProjectNotification {
  if (!d.uses.trim()) {
    throw new Error(`uses is required (on: ${d.on})`);
  }
  const withMap: Record<string, string> = {};
  for (const rawLine of d.withYaml.split("\n")) {
    const line = rawLine.trim();
    if (!line) continue;
    const colon = line.indexOf(":");
    if (colon < 0) {
      throw new Error(`malformed with line: ${line} (expected KEY: value)`);
    }
    const key = line.slice(0, colon).trim();
    let value = line.slice(colon + 1).trim();
    // Strip surrounding quotes the user might type out of YAML habit.
    if (
      (value.startsWith(`"`) && value.endsWith(`"`)) ||
      (value.startsWith(`'`) && value.endsWith(`'`))
    ) {
      value = value.slice(1, -1);
    }
    if (!key) {
      throw new Error(`malformed with line: ${line} (empty key)`);
    }
    withMap[key] = value;
  }
  const secrets = d.secrets
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);

  const out: ProjectNotification = { on: d.on, uses: d.uses.trim() };
  if (Object.keys(withMap).length > 0) out.with = withMap;
  if (secrets.length > 0) out.secrets = secrets;
  return out;
}
