"use client";

import { useState, useTransition } from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { setProjectFrameworks } from "@/server/actions/compliance";
import type { ComplianceFramework } from "@/server/queries/admin";

export function ProjectComplianceCard({
  slug,
  frameworks,
  assignedIDs,
}: {
  slug: string;
  frameworks: ComplianceFramework[];
  assignedIDs: string[];
}) {
  const [selected, setSelected] = useState<Set<string>>(new Set(assignedIDs));
  const [pending, startTransition] = useTransition();

  const toggle = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const save = () => {
    startTransition(async () => {
      const res = await setProjectFrameworks({
        slug,
        framework_ids: [...selected],
      });
      if (!res.ok) {
        // e.g. 409 when assigning a framework whose policy can't be enforced
        // on a project with no SCM source.
        toast.error(res.error);
        return;
      }
      toast.success("Compliance frameworks updated");
    });
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Compliance frameworks</CardTitle>
        <CardDescription>
          Admin only. Assigning a framework enforces every policy that targets it
          on this project — mandatory jobs that can&apos;t be removed from the
          repo. Requires a registered SCM source.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {frameworks.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No compliance frameworks defined yet.
          </p>
        ) : (
          <div className="flex flex-wrap gap-1.5">
            {frameworks.map((f) => {
              const on = selected.has(f.id);
              return (
                <button
                  key={f.id}
                  type="button"
                  onClick={() => toggle(f.id)}
                  aria-pressed={on}
                  aria-label={`Framework ${f.name}`}
                  disabled={pending}
                >
                  <Badge
                    variant={on ? "default" : "outline"}
                    className={cn("cursor-pointer", !on && "hover:bg-accent")}
                  >
                    {f.name}
                  </Badge>
                </button>
              );
            })}
          </div>
        )}
        <div className="flex justify-end">
          <Button size="sm" onClick={save} disabled={pending || frameworks.length === 0}>
            {pending ? <Loader2 className="mr-1.5 size-4 animate-spin" /> : null}
            Save frameworks
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
