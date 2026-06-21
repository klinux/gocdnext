"use client";

import { Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import type { AdminToleration } from "@/server/queries/admin";

// Label maps so the Select trigger renders the human label — base-ui's
// Select.Value shows the raw value otherwise. The empty-string key on
// EFFECT_LABELS is a real, selectable value ("any effect"); base-ui
// treats only `null` as "no selection", never "".
const OPERATOR_LABELS: Record<string, string> = {
  Equal: "Equal",
  Exists: "Exists",
};
const EFFECT_LABELS: Record<string, string> = {
  "": "any effect",
  NoSchedule: "NoSchedule",
  PreferNoSchedule: "PreferNoSchedule",
  NoExecute: "NoExecute",
};

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
        Profile values <em>win</em> over the agent&apos;s default on key collisions.
        Keys + values follow Kubernetes label rules.
      </p>
      {rows.map((row, i) => (
        <div key={i} className="flex flex-col gap-2 sm:flex-row">
          <Input
            value={row.key}
            placeholder="kubernetes.io/arch"
            onChange={(e) => updateRow(rows, setRows, i, { key: e.target.value })}
            className="flex-1 font-mono text-xs"
            aria-label="Node selector key"
          />
          <Input
            value={row.value}
            placeholder="amd64"
            onChange={(e) => updateRow(rows, setRows, i, { value: e.target.value })}
            className="flex-1 font-mono text-xs"
            aria-label="Node selector value"
          />
          <Button
            type="button"
            size="icon"
            variant="ghost"
            onClick={() => removeRow(rows, setRows, i)}
            aria-label="Remove node selector key"
            className="self-end sm:self-auto"
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
        Appended to the agent&apos;s tolerations. <code>toleration_seconds</code>{" "}
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
      {/*
        Mobile (< sm): one control per row (grid-cols-1). Above sm:
        12-col grid with the original spans. Keeps the editor usable
        on a phone-sized viewport without sacrificing the dense
        desktop layout.
      */}
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-12">
        <Input
          value={row.key ?? ""}
          placeholder="key (e.g. ci-only)"
          onChange={(e) => onChange({ key: e.target.value })}
          className="font-mono text-xs sm:col-span-4"
          aria-label="Toleration key"
        />
        <Select
          items={OPERATOR_LABELS}
          value={row.operator}
          onValueChange={(v) => {
            if (v === "Equal" || v === "Exists") onChange({ operator: v });
          }}
        >
          <SelectTrigger
            aria-label="Toleration operator"
            className="w-full text-xs sm:col-span-2"
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="Equal">Equal</SelectItem>
            <SelectItem value="Exists">Exists</SelectItem>
          </SelectContent>
        </Select>
        <Input
          value={row.value ?? ""}
          placeholder={isExists ? "(must be empty)" : "value"}
          onChange={(e) => onChange({ value: e.target.value })}
          disabled={isExists}
          className={cn(
            "font-mono text-xs sm:col-span-3",
            isExists && "bg-muted/60",
          )}
          aria-label="Toleration value"
        />
        <Select
          items={EFFECT_LABELS}
          value={row.effect ?? ""}
          onValueChange={(v) =>
            onChange({ effect: (v ?? "") as TolerationRow["effect"] })
          }
        >
          <SelectTrigger
            aria-label="Toleration effect"
            className="w-full text-xs sm:col-span-2"
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="">any effect</SelectItem>
            <SelectItem value="NoSchedule">NoSchedule</SelectItem>
            <SelectItem value="PreferNoSchedule">PreferNoSchedule</SelectItem>
            <SelectItem value="NoExecute">NoExecute</SelectItem>
          </SelectContent>
        </Select>
        <Button
          type="button"
          size="icon"
          variant="ghost"
          onClick={onRemove}
          aria-label="Remove toleration"
          className="justify-self-end sm:col-span-1 sm:justify-self-auto"
        >
          <Trash2 className="h-4 w-4" />
        </Button>
      </div>
      <div className="grid grid-cols-1 gap-2 text-xs text-muted-foreground sm:grid-cols-12">
        <label className="flex items-center gap-2 sm:col-span-6">
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
            aria-label="Toleration seconds (NoExecute only)"
          />
        </label>
        <span className="self-center sm:col-span-6">
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

// isEmptyTolerationRow recognises the "I clicked + Add but typed
// nothing" row — every field still on its default. Only THESE rows
// get silently dropped on save. Anything the user actually touched
// (a value typed, a non-default operator, an effect set, a seconds
// value entered) lands on the wire so the server returns the
// canonical error message instead of the row vanishing without
// explanation.
function isEmptyTolerationRow(r: TolerationRow): boolean {
  return (
    (r.key ?? "").trim() === "" &&
    r.operator === "Equal" &&
    (r.value ?? "") === "" &&
    (r.effect ?? "") === "" &&
    (r.toleration_seconds == null)
  );
}

export function collectTolerations(
  rows: TolerationRow[],
): CollectedToleration[] {
  return rows
    .filter((r) => !isEmptyTolerationRow(r))
    .map((r) => ({
      key: (r.key ?? "").trim(),
      operator: r.operator,
      value: r.value ?? "",
      effect: r.effect ?? "",
      toleration_seconds: r.toleration_seconds ?? null,
    }));
}
