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
import type {
  AdminNodeAffinityExpr,
  AdminPreferredNodeAffinityTerm,
} from "@/server/queries/admin";

const AFFINITY_OPERATORS = [
  "In",
  "NotIn",
  "Exists",
  "DoesNotExist",
  "Gt",
  "Lt",
] as const;
type AffinityOperator = (typeof AFFINITY_OPERATORS)[number];

// `values` is a LIST (one input per value), NOT a comma-joined string, so an
// empty label value round-trips: Kubernetes accepts `In`/`NotIn` with
// `values: [""]`, and a per-value list distinguishes `[""]` (one empty value)
// from `[]` (no values) — a comma string collapses both to "".
export type AffinityExprRow = {
  key: string;
  operator: string;
  values: string[];
};
export type AffinityTermRow = {
  weight: number;
  match_expressions: AffinityExprRow[];
};

function operatorTakesValues(op: string): boolean {
  return op !== "Exists" && op !== "DoesNotExist";
}
function emptyExpr(): AffinityExprRow {
  return { key: "", operator: "In", values: [""] };
}
function emptyTerm(): AffinityTermRow {
  return { weight: 100, match_expressions: [emptyExpr()] };
}

export function PreferredNodeAffinityEditor({
  rows,
  setRows,
}: {
  rows: AffinityTermRow[];
  setRows: (rows: AffinityTermRow[]) => void;
}) {
  const patchTerm = (ti: number, patch: Partial<AffinityTermRow>) =>
    setRows(rows.map((t, i) => (i === ti ? { ...t, ...patch } : t)));

  const patchExpr = (ti: number, ei: number, patch: Partial<AffinityExprRow>) =>
    patchTerm(ti, {
      match_expressions: (rows[ti]?.match_expressions ?? []).map((e, j) => {
        if (j !== ei) return e;
        const next = { ...e, ...patch };
        // Exists / DoesNotExist take no values (k8s rule); switching TO them
        // clears the list, switching AWAY re-seeds one empty input to type into.
        if (!operatorTakesValues(next.operator)) {
          next.values = [];
        } else if (next.values.length === 0) {
          next.values = [""];
        }
        return next;
      }),
    });

  return (
    <fieldset className="space-y-3 rounded-md border border-border p-3">
      <legend className="px-1 text-xs font-medium uppercase tracking-wider text-muted-foreground">
        Node affinity (preferred)
      </legend>
      <p className="text-xs text-muted-foreground">
        <em>Soft</em> preference: the scheduler leans toward matching nodes but
        the hard <strong>node selector</strong> still governs — an unsatisfiable
        preference simply has no effect. Use it to prefer a node class (e.g.
        spot) and fall back to another.
      </p>
      {rows.map((term, ti) => (
        <div
          key={ti}
          className="space-y-2 rounded-md border border-border/60 bg-muted/30 p-2"
        >
          <div className="flex items-center gap-2">
            <label className="flex items-center gap-2 text-xs text-muted-foreground">
              <span>weight</span>
              <Input
                value={String(term.weight)}
                onChange={(e) => {
                  const n = Number(e.target.value.trim());
                  if (Number.isFinite(n)) patchTerm(ti, { weight: Math.floor(n) });
                }}
                inputMode="numeric"
                placeholder="1-100"
                className="w-20 font-mono text-xs"
                aria-label="Affinity weight"
              />
            </label>
            <Button
              type="button"
              size="icon"
              variant="ghost"
              onClick={() => setRows(rows.filter((_, i) => i !== ti))}
              aria-label="Remove affinity term"
              className="ml-auto"
            >
              <Trash2 className="h-4 w-4" />
            </Button>
          </div>
          {term.match_expressions.map((expr, ei) => (
            <ExpressionRow
              key={ei}
              expr={expr}
              onPatch={(patch) => patchExpr(ti, ei, patch)}
              onRemove={() =>
                patchTerm(ti, {
                  match_expressions: term.match_expressions.filter(
                    (_, j) => j !== ei,
                  ),
                })
              }
            />
          ))}
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() =>
              patchTerm(ti, {
                match_expressions: [...term.match_expressions, emptyExpr()],
              })
            }
          >
            + Add expression
          </Button>
        </div>
      ))}
      <Button
        type="button"
        size="sm"
        variant="outline"
        onClick={() => setRows([...rows, emptyTerm()])}
      >
        + Add affinity term
      </Button>
    </fieldset>
  );
}

function ExpressionRow({
  expr,
  onPatch,
  onRemove,
}: {
  expr: AffinityExprRow;
  onPatch: (patch: Partial<AffinityExprRow>) => void;
  onRemove: () => void;
}) {
  const takesValues = operatorTakesValues(expr.operator);
  return (
    <div className="grid grid-cols-1 gap-2 sm:grid-cols-12">
      <Input
        value={expr.key}
        placeholder="cloud.google.com/gke-spot"
        onChange={(e) => onPatch({ key: e.target.value })}
        className="font-mono text-xs sm:col-span-4"
        aria-label="Affinity key"
      />
      <Select
        value={expr.operator}
        onValueChange={(v) => {
          if (v) onPatch({ operator: v as AffinityOperator });
        }}
      >
        <SelectTrigger
          aria-label="Affinity operator"
          className="w-full text-xs sm:col-span-3"
        >
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {AFFINITY_OPERATORS.map((op) => (
            <SelectItem key={op} value={op}>
              {op}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <div className="flex flex-col gap-1 sm:col-span-4">
        {takesValues ? (
          <>
            {expr.values.map((val, vi) => (
              <div key={vi} className="flex items-center gap-1">
                <Input
                  value={val}
                  placeholder="true"
                  onChange={(e) => {
                    const values = expr.values.slice();
                    values[vi] = e.target.value;
                    onPatch({ values });
                  }}
                  className="flex-1 font-mono text-xs"
                  aria-label="Affinity value"
                />
                <Button
                  type="button"
                  size="icon"
                  variant="ghost"
                  onClick={() =>
                    onPatch({ values: expr.values.filter((_, j) => j !== vi) })
                  }
                  aria-label="Remove affinity value"
                >
                  <Trash2 className="h-3.5 w-3.5" />
                </Button>
              </div>
            ))}
            <Button
              type="button"
              size="sm"
              variant="ghost"
              className="self-start text-xs"
              onClick={() => onPatch({ values: [...expr.values, ""] })}
            >
              + value
            </Button>
          </>
        ) : (
          <span className="self-center text-xs text-muted-foreground">
            (no values)
          </span>
        )}
      </div>
      <Button
        type="button"
        size="icon"
        variant="ghost"
        onClick={onRemove}
        aria-label="Remove affinity expression"
        className="justify-self-end sm:col-span-1 sm:justify-self-auto"
      >
        <Trash2 className="h-4 w-4" />
      </Button>
    </div>
  );
}

// affinityTermsFromApi hydrates the editor rows from the API shape. Values are
// copied VERBATIM (preserving `[""]`, `[]`, and multiple entries) — no join, no
// filter — so an API/seed-authored profile round-trips without alteration.
export function affinityTermsFromApi(
  terms: AdminPreferredNodeAffinityTerm[] | undefined,
): AffinityTermRow[] {
  if (!terms) return [];
  return terms.map((t) => ({
    weight: t.weight,
    match_expressions: (t.match_expressions ?? []).map((e) => ({
      key: e.key,
      operator: e.operator,
      values: [...(e.values ?? [])],
    })),
  }));
}

// Collected* are the save-time shapes with `values` always present so they
// satisfy both the read type (optional values) and the write action.
export type CollectedAffinityExpr = {
  key: string;
  operator: AdminNodeAffinityExpr["operator"];
  values: string[];
};
export type CollectedAffinityTerm = {
  weight: number;
  match_expressions: CollectedAffinityExpr[];
};

// collectPreferredNodeAffinity is the form → API transform at save: trims each
// value but does NOT drop empties (so an intentional `[""]` survives), forces
// `[]` for Exists/DoesNotExist, and drops only expressions with no key and
// terms left with no usable expression.
export function collectPreferredNodeAffinity(
  rows: AffinityTermRow[],
): CollectedAffinityTerm[] {
  const out: CollectedAffinityTerm[] = [];
  for (const term of rows) {
    const exprs: CollectedAffinityExpr[] = term.match_expressions
      .map((e) => ({
        key: e.key.trim(),
        operator: e.operator as AdminNodeAffinityExpr["operator"],
        values: operatorTakesValues(e.operator)
          ? e.values.map((v) => v.trim())
          : [],
      }))
      .filter((e) => e.key !== "");
    if (exprs.length === 0) continue;
    out.push({ weight: term.weight, match_expressions: exprs });
  }
  return out;
}
