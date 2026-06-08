"use client";

import { Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import type { AdminToleration } from "@/server/queries/admin";

// NodeSelectorRow + TolerationRow are the editor-local shapes:
// rows carry an optional empty pair the user is editing into. The
// parent collapses empty rows on save so an "I clicked Add but
// didn't fill in anything" row doesn't trip server-side validation.
export type NodeSelectorRow = { key: string; value: string };
export type TolerationRow = AdminToleration;

type Props = {
  nodeSelector: NodeSelectorRow[];
  setNodeSelector: (rows: NodeSelectorRow[]) => void;
  tolerations: TolerationRow[];
  setTolerations: (rows: TolerationRow[]) => void;
};

// SchedulingFields renders the two scheduling editors as a self-
// contained block inside the profile form. Kept in its own file so
// the profile manager stays under the per-file LOC budget.
export function SchedulingFields({
  nodeSelector,
  setNodeSelector,
  tolerations,
  setTolerations,
}: Props) {
  return (
    <div className="space-y-4">
      <NodeSelectorEditor rows={nodeSelector} setRows={setNodeSelector} />
      <TolerationsEditor rows={tolerations} setRows={setTolerations} />
    </div>
  );
}

function NodeSelectorEditor({
  rows,
  setRows,
}: {
  rows: NodeSelectorRow[];
  setRows: (rows: NodeSelectorRow[]) => void;
}) {
  return (
    <fieldset className="space-y-2 rounded-md border border-border p-3">
      <legend className="px-1 text-xs font-medium uppercase tracking-wider text-muted-foreground">
        Node selector
      </legend>
      <p className="text-xs text-muted-foreground">
        Profile values <em>win</em> over the agent's default on key collisions.
        Keys + values follow Kubernetes label rules.
      </p>
      {rows.map((row, i) => (
        <div key={i} className="flex gap-2">
          <Input
            value={row.key}
            placeholder="kubernetes.io/arch"
            onChange={(e) => updateRow(rows, setRows, i, { key: e.target.value })}
            className="flex-1 font-mono text-xs"
          />
          <Input
            value={row.value}
            placeholder="amd64"
            onChange={(e) => updateRow(rows, setRows, i, { value: e.target.value })}
            className="flex-1 font-mono text-xs"
          />
          <Button
            type="button"
            size="icon"
            variant="ghost"
            onClick={() => removeRow(rows, setRows, i)}
            aria-label="Remove key"
          >
            <Trash2 className="h-4 w-4" />
          </Button>
        </div>
      ))}
      <Button
        type="button"
        size="sm"
        variant="outline"
        onClick={() => setRows([...rows, { key: "", value: "" }])}
      >
        + Add label
      </Button>
    </fieldset>
  );
}

function TolerationsEditor({
  rows,
  setRows,
}: {
  rows: TolerationRow[];
  setRows: (rows: TolerationRow[]) => void;
}) {
  return (
    <fieldset className="space-y-3 rounded-md border border-border p-3">
      <legend className="px-1 text-xs font-medium uppercase tracking-wider text-muted-foreground">
        Tolerations
      </legend>
      <p className="text-xs text-muted-foreground">
        Appended to the agent's tolerations. <code>toleration_seconds</code>{" "}
        is honoured only when <code>effect</code> is <code>NoExecute</code>.
      </p>
      {rows.map((row, i) => (
        <TolerationEditorRow
          key={i}
          row={row}
          onChange={(patch) => updateTolerationRow(rows, setRows, i, patch)}
          onRemove={() => removeTolerationRow(rows, setRows, i)}
        />
      ))}
      <Button
        type="button"
        size="sm"
        variant="outline"
        onClick={() =>
          setRows([
            ...rows,
            { key: "", operator: "Equal", value: "", effect: "" },
          ])
        }
      >
        + Add toleration
      </Button>
    </fieldset>
  );
}

function TolerationEditorRow({
  row,
  onChange,
  onRemove,
}: {
  row: TolerationRow;
  onChange: (patch: Partial<TolerationRow>) => void;
  onRemove: () => void;
}) {
  const isExists = row.operator === "Exists";
  // Server rejects toleration_seconds with effect ≠ NoExecute; keep
  // the field disabled until the user picks NoExecute so the form
  // can't submit a value the server will reject.
  const secondsAllowed = row.effect === "NoExecute";
  return (
    <div className="space-y-2 rounded-md border border-border/60 bg-muted/30 p-2">
      <div className="grid grid-cols-12 gap-2">
        <Input
          value={row.key ?? ""}
          placeholder="key (e.g. ci-only)"
          onChange={(e) => onChange({ key: e.target.value })}
          className="col-span-4 font-mono text-xs"
        />
        <select
          value={row.operator}
          onChange={(e) =>
            onChange({ operator: e.target.value as "Equal" | "Exists" })
          }
          className="col-span-2 rounded-md border border-input bg-background px-2 text-xs"
        >
          <option value="Equal">Equal</option>
          <option value="Exists">Exists</option>
        </select>
        <Input
          value={row.value ?? ""}
          placeholder={isExists ? "(must be empty)" : "value"}
          onChange={(e) => onChange({ value: e.target.value })}
          disabled={isExists}
          className={cn(
            "col-span-3 font-mono text-xs",
            isExists && "bg-muted/60",
          )}
        />
        <select
          value={row.effect ?? ""}
          onChange={(e) =>
            onChange({ effect: e.target.value as TolerationRow["effect"] })
          }
          className="col-span-2 rounded-md border border-input bg-background px-2 text-xs"
        >
          <option value="">any effect</option>
          <option value="NoSchedule">NoSchedule</option>
          <option value="PreferNoSchedule">PreferNoSchedule</option>
          <option value="NoExecute">NoExecute</option>
        </select>
        <Button
          type="button"
          size="icon"
          variant="ghost"
          onClick={onRemove}
          aria-label="Remove toleration"
          className="col-span-1"
        >
          <Trash2 className="h-4 w-4" />
        </Button>
      </div>
      <div className="grid grid-cols-12 gap-2 text-xs text-muted-foreground">
        <label className="col-span-6 flex items-center gap-2">
          <span className="whitespace-nowrap">toleration_seconds</span>
          <Input
            value={
              row.toleration_seconds == null
                ? ""
                : String(row.toleration_seconds)
            }
            placeholder={secondsAllowed ? "60" : "NoExecute only"}
            onChange={(e) => {
              const v = e.target.value.trim();
              if (v === "") {
                onChange({ toleration_seconds: null });
                return;
              }
              const n = Number(v);
              if (Number.isFinite(n) && n >= 0) {
                onChange({ toleration_seconds: Math.floor(n) });
              }
            }}
            disabled={!secondsAllowed}
            className={cn(
              "flex-1 font-mono",
              !secondsAllowed && "bg-muted/60",
            )}
            inputMode="numeric"
          />
        </label>
        <span className="col-span-6 self-center">
          {isExists ? "Exists matches any value" : null}
        </span>
      </div>
    </div>
  );
}

function updateRow(
  rows: NodeSelectorRow[],
  setRows: (rows: NodeSelectorRow[]) => void,
  i: number,
  patch: Partial<NodeSelectorRow>,
) {
  setRows(rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r)));
}

function removeRow(
  rows: NodeSelectorRow[],
  setRows: (rows: NodeSelectorRow[]) => void,
  i: number,
) {
  setRows(rows.filter((_, idx) => idx !== i));
}

function updateTolerationRow(
  rows: TolerationRow[],
  setRows: (rows: TolerationRow[]) => void,
  i: number,
  patch: Partial<TolerationRow>,
) {
  setRows(
    rows.map((r, idx) => {
      if (idx !== i) return r;
      const next = { ...r, ...patch };
      // Cross-field invariant kept client-side so the form matches
      // the server rule: Exists forces empty value; switching away
      // from NoExecute clears toleration_seconds.
      if (next.operator === "Exists") {
        next.value = "";
      }
      if (next.effect !== "NoExecute") {
        next.toleration_seconds = null;
      }
      return next;
    }),
  );
}

function removeTolerationRow(
  rows: TolerationRow[],
  setRows: (rows: TolerationRow[]) => void,
  i: number,
) {
  setRows(rows.filter((_, idx) => idx !== i));
}

// collectNodeSelector + collectTolerations are the form → API
// transforms used at save time: drop empty rows, return the
// JSON shape the server action accepts.
export function collectNodeSelector(
  rows: NodeSelectorRow[],
): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows) {
    const k = r.key.trim();
    if (k) out[k] = r.value;
  }
  return out;
}

// CollectedToleration narrows AdminToleration to the strict shape
// the server action's Zod schema expects: every field present, key
// always a string (collapsed from undefined to ""), and
// operator/effect coerced to non-empty strings (the server
// normalises empty operator → Equal, but we send the explicit form
// to keep client/server semantics symmetric).
export type CollectedToleration = {
  key: string;
  operator: "Equal" | "Exists";
  value: string;
  effect: "" | "NoSchedule" | "PreferNoSchedule" | "NoExecute";
  toleration_seconds: number | null;
};

export function collectTolerations(
  rows: TolerationRow[],
): CollectedToleration[] {
  return rows
    .filter((r) => (r.key ?? "").trim() !== "" || r.operator === "Exists")
    .map((r) => ({
      key: (r.key ?? "").trim(),
      operator: r.operator,
      value: r.value ?? "",
      effect: r.effect ?? "",
      toleration_seconds: r.toleration_seconds ?? null,
    }));
}
