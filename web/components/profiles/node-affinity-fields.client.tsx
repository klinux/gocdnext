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

// Editor-local shapes. `values` is a comma-separated string while
// editing (parsed into a list on save) so a term can carry multiple
// values without a nested list-of-inputs. A term keeps a LIST of
// expressions — the API + seed accept several (AND-ed), so the editor
// must too, or editing a YAML-authored profile would silently drop all
// but the first expression.
export type AffinityExprRow = { key: string; operator: string; values: string };
export type AffinityTermRow = {
  weight: number;
  match_expressions: AffinityExprRow[];
};

function emptyExpr(): AffinityExprRow {
  return { key: "", operator: "In", values: "" };
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
    setRows(
      rows.map((t, i) =>
        i === ti
          ? {
              ...t,
              match_expressions: t.match_expressions.map((e, j) => {
                if (j !== ei) return e;
                const next = { ...e, ...patch };
                // Exists / DoesNotExist take no values (k8s rule); keep
                // the field cleared so the form can't submit a value the
                // server would reject.
                if (next.operator === "Exists" || next.operator === "DoesNotExist") {
                  next.values = "";
                }
                return next;
              }),
            }
          : t,
      ),
    );

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
          {term.match_expressions.map((expr, ei) => {
            const noValues =
              expr.operator === "Exists" || expr.operator === "DoesNotExist";
            return (
              <div key={ei} className="grid grid-cols-1 gap-2 sm:grid-cols-12">
                <Input
                  value={expr.key}
                  placeholder="cloud.google.com/gke-spot"
                  onChange={(e) => patchExpr(ti, ei, { key: e.target.value })}
                  className="font-mono text-xs sm:col-span-4"
                  aria-label="Affinity key"
                />
                <Select
                  value={expr.operator}
                  onValueChange={(v) => {
                    if (v) patchExpr(ti, ei, { operator: v as AffinityOperator });
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
                <Input
                  value={expr.values}
                  placeholder={noValues ? "(no values)" : "true (comma-sep)"}
                  onChange={(e) => patchExpr(ti, ei, { values: e.target.value })}
                  disabled={noValues}
                  className={cn(
                    "font-mono text-xs sm:col-span-4",
                    noValues && "bg-muted/60",
                  )}
                  aria-label="Affinity values (comma-separated)"
                />
                <Button
                  type="button"
                  size="icon"
                  variant="ghost"
                  onClick={() =>
                    patchTerm(ti, {
                      match_expressions: term.match_expressions.filter(
                        (_, j) => j !== ei,
                      ),
                    })
                  }
                  aria-label="Remove affinity expression"
                  className="justify-self-end sm:col-span-1 sm:justify-self-auto"
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>
            );
          })}
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

// affinityTermsFromApi hydrates the editor rows from the API shape,
// joining each expression's values into the comma-separated string the
// editor edits. Preserves EVERY expression (no silent truncation).
export function affinityTermsFromApi(
  terms: AdminPreferredNodeAffinityTerm[] | undefined,
): AffinityTermRow[] {
  if (!terms) return [];
  return terms.map((t) => ({
    weight: t.weight,
    match_expressions: (t.match_expressions ?? []).map((e) => ({
      key: e.key,
      operator: e.operator,
      values: (e.values ?? []).join(", "),
    })),
  }));
}

// collectPreferredNodeAffinity is the form → API transform at save:
// trims, parses comma-separated values, drops expressions with no key
// and terms left with no usable expression.
// Collected* are the save-time shapes with `values` always present (never
// undefined) so they satisfy both the read type (optional values) and the
// write action's required-values input.
export type CollectedAffinityExpr = {
  key: string;
  operator: AdminNodeAffinityExpr["operator"];
  values: string[];
};
export type CollectedAffinityTerm = {
  weight: number;
  match_expressions: CollectedAffinityExpr[];
};

export function collectPreferredNodeAffinity(
  rows: AffinityTermRow[],
): CollectedAffinityTerm[] {
  const out: CollectedAffinityTerm[] = [];
  for (const term of rows) {
    const exprs: CollectedAffinityExpr[] = term.match_expressions
      .map((e) => ({
        key: e.key.trim(),
        operator: e.operator as AdminNodeAffinityExpr["operator"],
        values: e.values
          .split(",")
          .map((v) => v.trim())
          .filter((v) => v !== ""),
      }))
      .filter((e) => e.key !== "");
    if (exprs.length === 0) continue;
    out.push({ weight: term.weight, match_expressions: exprs });
  }
  return out;
}
