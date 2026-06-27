"use client";

import { type Dispatch, type SetStateAction, useMemo, useState, useTransition } from "react";
import { Pencil, Plus, Search, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Sheet, SheetContent } from "@/components/ui/sheet";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  createCompliancePolicy,
  deleteCompliancePolicy,
  updateCompliancePolicy,
} from "@/server/actions/compliance";
import type { ComplianceFramework, CompliancePolicy } from "@/server/queries/admin";
import { blankPolicy, policyToDraft, type PolicyDraft } from "./policy-form.client";
import { PolicySheet } from "./policy-sheet.client";
import type { PreviewProject } from "./use-policy-preview";

export function PoliciesManager({
  policies,
  setPolicies,
  frameworks,
  projects,
}: {
  policies: CompliancePolicy[];
  setPolicies: Dispatch<SetStateAction<CompliancePolicy[]>>;
  frameworks: ComplianceFramework[];
  projects: PreviewProject[];
}) {
  const [filter, setFilter] = useState("");
  const [draft, setDraft] = useState<PolicyDraft | null>(null);
  const [pending, startTransition] = useTransition();

  const fwName = useMemo(() => {
    const m = new Map<string, string>();
    for (const f of frameworks) m.set(f.id, f.name);
    return m;
  }, [frameworks]);

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return policies;
    return policies.filter(
      (p) =>
        p.name.toLowerCase().includes(q) ||
        p.description.toLowerCase().includes(q),
    );
  }, [policies, filter]);

  function save() {
    if (!draft) return;
    if (!draft.name.trim()) {
      toast.error("Name is required");
      return;
    }
    if (!draft.configYaml.trim()) {
      toast.error("Policy config (YAML) is required");
      return;
    }
    const body = {
      name: draft.name.trim(),
      description: draft.description,
      enabled: draft.enabled,
      mode: draft.mode,
      priority: draft.priority,
      applies_to_all: draft.appliesToAll,
      position_before: draft.positionBefore,
      position_after: draft.positionAfter,
      framework_ids: draft.appliesToAll ? [] : draft.frameworkIds,
      config_yaml: draft.configYaml,
    };
    startTransition(async () => {
      if (draft.id) {
        const res = await updateCompliancePolicy({ ...body, id: draft.id });
        if (!res.ok) {
          toast.error(res.error);
          return;
        }
        const id = draft.id;
        setPolicies((prev) =>
          prev.map((p) =>
            p.id === id
              ? { ...p, ...body, id, framework_ids: body.framework_ids }
              : p,
          ),
        );
        toast.success("Policy updated");
      } else {
        const res = await createCompliancePolicy(body);
        if (!res.ok) {
          toast.error(res.error);
          return;
        }
        // Use the persisted DTO (real id) so the row is immediately editable.
        setPolicies((prev) =>
          [...prev, res.data].sort(
            (a, b) => a.priority - b.priority || a.name.localeCompare(b.name),
          ),
        );
        toast.success("Policy created");
      }
      setDraft(null);
    });
  }

  function remove(p: CompliancePolicy) {
    if (!confirm(`Delete policy "${p.name}"?`)) return;
    startTransition(async () => {
      const res = await deleteCompliancePolicy(p.id);
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success("Policy deleted");
      setPolicies((prev) => prev.filter((x) => x.id !== p.id));
    });
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-4">
        <div className="relative max-w-sm flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter policies…"
            className="pl-8"
          />
        </div>
        <Button size="sm" onClick={() => setDraft(blankPolicy())}>
          <Plus className="mr-1.5 size-4" /> New policy
        </Button>
      </div>

      <div className="overflow-hidden rounded-lg border border-border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Scope</TableHead>
              <TableHead>Mode</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="w-[100px]" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {filtered.length === 0 ? (
              <TableRow>
                <TableCell
                  colSpan={5}
                  className="py-8 text-center text-sm text-muted-foreground"
                >
                  {policies.length === 0
                    ? "No policies yet — create one to enforce mandatory jobs."
                    : "No policies match the filter."}
                </TableCell>
              </TableRow>
            ) : (
              filtered.map((p) => (
                <TableRow key={p.id}>
                  <TableCell className="font-medium">
                    {p.name}
                    {p.description ? (
                      <span className="block text-xs font-normal text-muted-foreground">
                        {p.description}
                      </span>
                    ) : null}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {p.applies_to_all ? (
                      <Badge variant="secondary">all projects</Badge>
                    ) : p.framework_ids.length ? (
                      <span className="flex flex-wrap gap-1">
                        {p.framework_ids.map((id) => (
                          <Badge key={id} variant="outline">
                            {fwName.get(id) ?? id}
                          </Badge>
                        ))}
                      </span>
                    ) : (
                      <span className="italic">no targets</span>
                    )}
                  </TableCell>
                  <TableCell className="text-xs">{p.mode}</TableCell>
                  <TableCell>
                    <Badge variant={p.enabled ? "default" : "secondary"}>
                      {p.enabled ? "enabled" : "disabled"}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() => setDraft(policyToDraft(p))}
                      aria-label={`Edit ${p.name}`}
                    >
                      <Pencil className="size-4" />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() => remove(p)}
                      disabled={pending}
                      aria-label={`Delete ${p.name}`}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>

      <Sheet open={draft !== null} onOpenChange={(o) => !o && setDraft(null)}>
        <SheetContent
          side="right"
          className="gap-0 p-0 data-[side=right]:w-full data-[side=right]:sm:max-w-[60rem]"
        >
          {draft ? (
            <PolicySheet
              draft={draft}
              setDraft={setDraft}
              frameworks={frameworks}
              projects={projects}
              pending={pending}
              onSave={save}
              onCancel={() => setDraft(null)}
            />
          ) : null}
        </SheetContent>
      </Sheet>
    </div>
  );
}
