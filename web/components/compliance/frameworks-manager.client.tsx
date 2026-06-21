"use client";

import { useMemo, useState, useTransition } from "react";
import { Loader2, Pencil, Plus, Search, Trash2, X } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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
import {
  createComplianceFramework,
  deleteComplianceFramework,
  updateComplianceFramework,
} from "@/server/actions/compliance";
import type { ComplianceFramework } from "@/server/queries/admin";

type Draft = { id: string | null; name: string; description: string };

function blank(): Draft {
  return { id: null, name: "", description: "" };
}

export function FrameworksManager({
  frameworks,
}: {
  frameworks: ComplianceFramework[];
}) {
  const [items, setItems] = useState(frameworks);
  const [filter, setFilter] = useState("");
  const [draft, setDraft] = useState<Draft | null>(null);
  const [pending, startTransition] = useTransition();

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return items;
    return items.filter(
      (f) =>
        f.name.toLowerCase().includes(q) ||
        f.description.toLowerCase().includes(q),
    );
  }, [items, filter]);

  function save() {
    if (!draft) return;
    const name = draft.name.trim();
    if (!name) {
      toast.error("Name is required");
      return;
    }
    startTransition(async () => {
      const body = { name, description: draft.description };
      const res = draft.id
        ? await updateComplianceFramework({ ...body, id: draft.id })
        : await createComplianceFramework(body);
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success(draft.id ? "Framework updated" : "Framework created");
      const now = new Date().toISOString();
      const next: ComplianceFramework = {
        id: draft.id ?? `__opt_${now}`,
        name,
        description: draft.description,
        created_by: "",
        created_at: now,
        updated_at: now,
      };
      setItems((prev) =>
        draft.id
          ? prev.map((f) => (f.id === draft.id ? { ...f, ...next, id: f.id } : f))
          : [...prev, next].sort((a, b) => a.name.localeCompare(b.name)),
      );
      setDraft(null);
    });
  }

  function remove(f: ComplianceFramework) {
    if (!confirm(`Delete framework "${f.name}"?`)) return;
    startTransition(async () => {
      const res = await deleteComplianceFramework(f.id);
      if (!res.ok) {
        // Surfaces the server's 409 "framework in use" with project/policy counts.
        toast.error(res.error);
        return;
      }
      toast.success("Framework deleted");
      setItems((prev) => prev.filter((x) => x.id !== f.id));
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
            placeholder="Filter frameworks…"
            className="pl-8"
          />
        </div>
        <Button size="sm" onClick={() => setDraft(blank())}>
          <Plus className="mr-1.5 size-4" /> New framework
        </Button>
      </div>

      <div className="overflow-hidden rounded-lg border border-border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Description</TableHead>
              <TableHead className="w-[100px]" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {filtered.length === 0 ? (
              <TableRow>
                <TableCell
                  colSpan={3}
                  className="py-8 text-center text-sm text-muted-foreground"
                >
                  {items.length === 0
                    ? "No frameworks yet — create one to start enforcing policies."
                    : "No frameworks match the filter."}
                </TableCell>
              </TableRow>
            ) : (
              filtered.map((f) => (
                <TableRow key={f.id}>
                  <TableCell className="font-medium">{f.name}</TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {f.description}
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() =>
                        setDraft({
                          id: f.id,
                          name: f.name,
                          description: f.description,
                        })
                      }
                      aria-label={`Edit ${f.name}`}
                    >
                      <Pencil className="size-4" />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() => remove(f)}
                      disabled={pending}
                      aria-label={`Delete ${f.name}`}
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
        <SheetContent side="right" className="overflow-y-auto">
          <SheetHeader>
            <SheetTitle>{draft?.id ? "Edit framework" : "New framework"}</SheetTitle>
            <SheetDescription>
              A framework is a label assigned to projects; policies target it.
            </SheetDescription>
          </SheetHeader>
          {draft ? (
            <div className="grid gap-4 px-4 pb-6">
              <div className="grid gap-1.5">
                <Label htmlFor="fw-name">Name</Label>
                <Input
                  id="fw-name"
                  value={draft.name}
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                  placeholder="SOC2"
                  autoFocus
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="fw-desc">Description</Label>
                <Input
                  id="fw-desc"
                  value={draft.description}
                  onChange={(e) =>
                    setDraft({ ...draft, description: e.target.value })
                  }
                  placeholder="What this framework covers"
                />
              </div>
              <div className="mt-2 flex items-center justify-end gap-2">
                <Button variant="ghost" onClick={() => setDraft(null)} disabled={pending}>
                  <X className="mr-1.5 size-4" /> Cancel
                </Button>
                <Button onClick={save} disabled={pending}>
                  {pending ? <Loader2 className="mr-1.5 size-4 animate-spin" /> : null}
                  Save
                </Button>
              </div>
            </div>
          ) : null}
        </SheetContent>
      </Sheet>
    </div>
  );
}
