"use client";

import { useState, useTransition } from "react";
import { Loader2, Plus, Save, Tag, X } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { setProjectLabels } from "@/server/actions/project-settings";
import type { ProjectLabel } from "@/types/api";

type Row = { key: string; value: string };

export function ProjectLabelsCard({
  slug,
  initial,
}: {
  slug: string;
  initial: ProjectLabel[];
}) {
  const [rows, setRows] = useState<Row[]>(
    initial.length ? initial.map((l) => ({ key: l.key, value: l.value })) : [],
  );
  const [pending, startTransition] = useTransition();

  const update = (i: number, patch: Partial<Row>) =>
    setRows((prev) => prev.map((r, idx) => (idx === i ? { ...r, ...patch } : r)));
  const remove = (i: number) => setRows((prev) => prev.filter((_, idx) => idx !== i));
  const add = () => setRows((prev) => [...prev, { key: "", value: "" }]);

  const save = () => {
    // Drop wholly blank rows; a value without a key is ambiguous and should be
    // surfaced instead of silently disappearing on save.
    const trimmed = rows.map((r) => ({ key: r.key.trim(), value: r.value.trim() }));
    if (trimmed.some((r) => r.key === "" && r.value !== "")) {
      toast.error("Label key is required");
      return;
    }
    const labels = trimmed.filter((r) => r.key !== "");
    startTransition(async () => {
      const res = await setProjectLabels({ slug, labels });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success("Labels saved");
    });
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Tag className="size-4" /> Labels
        </CardTitle>
        <CardDescription>
          Free-form <span className="font-mono">key:value</span> tags
          (<span className="font-mono">team:payments</span>,{" "}
          <span className="font-mono">tier:critical</span>) used to group and
          filter projects, and to roll up cross-project analytics. Value is
          optional.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {rows.length === 0 ? (
          <p className="text-sm text-muted-foreground">No labels yet.</p>
        ) : (
          <div className="space-y-2">
            {rows.map((r, i) => (
              <div key={i} className="flex items-center gap-2">
                <Input
                  aria-label={`Label ${i + 1} key`}
                  value={r.key}
                  onChange={(e) => update(i, { key: e.target.value })}
                  placeholder="team"
                  className="font-mono"
                />
                <span className="text-muted-foreground">:</span>
                <Input
                  aria-label={`Label ${i + 1} value`}
                  value={r.value}
                  onChange={(e) => update(i, { value: e.target.value })}
                  placeholder="payments"
                  className="font-mono"
                />
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => remove(i)}
                  aria-label={`Remove label ${i + 1}`}
                >
                  <X className="size-4" />
                </Button>
              </div>
            ))}
          </div>
        )}
        <div className="flex items-center justify-between">
          <Button variant="outline" size="sm" onClick={add} disabled={pending}>
            <Plus className="mr-1.5 size-4" /> Add label
          </Button>
          <Button size="sm" onClick={save} disabled={pending}>
            {pending ? (
              <Loader2 className="mr-1.5 size-4 animate-spin" />
            ) : (
              <Save className="mr-1.5 size-4" />
            )}
            Save labels
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
