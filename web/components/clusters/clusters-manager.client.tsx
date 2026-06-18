"use client";

import { useMemo, useState, useTransition } from "react";
import { Pencil, Plus, PlugZap, Search, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
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
import { CREDENTIAL_PRESERVE_SENTINEL } from "@/lib/clusters";
import {
  createCluster,
  deleteCluster,
  testCluster,
  updateCluster,
} from "@/server/actions/clusters";
import type { AdminCluster } from "@/server/queries/admin";
import {
  AUTH_LABELS,
  ClusterForm,
  blankForm,
  clusterToDraft,
  type FormDraft,
  type ProjectOption,
} from "./cluster-form.client";

export type { ProjectOption };

type Props = {
  initial: AdminCluster[];
  // Project id → friendly name, for the allow-list picker and the
  // table's "#allowed" tooltip. Empty list = the picker shows nothing
  // (no projects yet) and the table falls back to raw ids.
  projects: ProjectOption[];
};

export function ClustersManager({ initial, projects }: Props) {
  const [clusters, setClusters] = useState<AdminCluster[]>(initial);
  const [filter, setFilter] = useState("");
  const [form, setForm] = useState<FormDraft | null>(null);
  const [pending, startTransition] = useTransition();

  const projectName = useMemo(() => {
    const m = new Map<string, string>();
    for (const p of projects) m.set(p.id, p.name);
    return m;
  }, [projects]);

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return clusters;
    return clusters.filter(
      (c) =>
        c.name.toLowerCase().includes(q) ||
        c.description.toLowerCase().includes(q) ||
        c.api_server.toLowerCase().includes(q) ||
        c.auth_type.toLowerCase().includes(q),
    );
  }, [clusters, filter]);

  const saveForm = () => {
    if (!form) return;
    const name = form.name.trim();
    if (!name) {
      toast.error("Name is required");
      return;
    }
    // On edit, a blank credential means "keep the stored one" → send the
    // preserve sentinel. On create it must be a real value (server
    // validates per auth_type; in_cluster needs neither field). ca_cert
    // is a public cert, prefilled on edit, so it's re-sent verbatim — a
    // metadata-only token edit must not drop it (server rejects a CA-less
    // token cluster).
    const credential =
      form.id && form.credential.trim() === ""
        ? CREDENTIAL_PRESERVE_SENTINEL
        : form.credential;

    startTransition(async () => {
      const body = {
        name,
        description: form.description,
        auth_type: form.auth_type,
        api_server: form.auth_type === "token" ? form.api_server : "",
        ca_cert: form.auth_type === "token" ? form.ca_cert : "",
        credential: form.auth_type === "in_cluster" ? "" : credential,
        allowed_projects: form.allowedProjects,
      };
      const res = form.id
        ? await updateCluster({ ...body, id: form.id })
        : await createCluster(body);
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success(form.id ? "Cluster updated" : "Cluster created");
      setForm(null);
      // Optimistic local row; the next server render replaces it with
      // the canonical record (timestamps, created_by). No secret bytes
      // live in the row — ca_cert is a public cert, credential never.
      const draft: AdminCluster = {
        id: form.id ?? "__opt__" + Date.now(),
        name,
        description: form.description,
        auth_type: form.auth_type,
        api_server: body.api_server,
        has_ca: body.ca_cert.trim().length > 0,
        ca_cert: body.ca_cert,
        allowed_projects: form.allowedProjects,
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
      };
      setClusters((prev) => {
        if (form.id) return prev.map((c) => (c.id === form.id ? draft : c));
        return [...prev, draft].sort((a, b) => a.name.localeCompare(b.name));
      });
    });
  };

  const handleDelete = (c: AdminCluster) => {
    if (
      !confirm(
        `Delete cluster "${c.name}"? Pipelines targeting it will fail to schedule until rewired.`,
      )
    ) {
      return;
    }
    startTransition(async () => {
      const res = await deleteCluster({ id: c.id });
      if (!res.ok) {
        // 409 (still referenced) surfaces the server's message verbatim.
        toast.error(res.error);
        return;
      }
      toast.success("Cluster deleted");
      setClusters((prev) => prev.filter((x) => x.id !== c.id));
    });
  };

  const handleTest = (c: AdminCluster) => {
    startTransition(async () => {
      const res = await testCluster({ id: c.id });
      if (!res.ok) {
        toast.error(`${c.name}: ${res.error}`);
        return;
      }
      // ok / skipped are not failures; everything else is.
      const { status, message } = res.probe;
      if (status === "ok") toast.success(`${c.name}: ${message}`);
      else if (status === "skipped") toast.info(`${c.name}: ${message}`);
      else toast.error(`${c.name}: ${message}`);
    });
  };

  return (
    <>
      <div className="flex items-center justify-between gap-4">
        <div className="relative max-w-sm flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
          <Input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter clusters…"
            className="pl-8"
          />
        </div>
        <Button onClick={() => setForm(blankForm())}>
          <Plus className="mr-2 h-4 w-4" /> New cluster
        </Button>
      </div>

      <div className="overflow-hidden rounded-lg border border-border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Auth</TableHead>
              <TableHead>API server</TableHead>
              <TableHead>Projects</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="w-[120px]" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {filtered.length === 0 ? (
              <TableRow>
                <TableCell
                  colSpan={6}
                  className="py-8 text-center text-sm text-muted-foreground"
                >
                  {clusters.length === 0
                    ? "No clusters registered yet — add one above."
                    : "No clusters match the filter."}
                </TableCell>
              </TableRow>
            ) : (
              filtered.map((c) => (
                <TableRow key={c.id}>
                  <TableCell className="font-medium">
                    {c.name}
                    {c.description ? (
                      <div className="text-xs text-muted-foreground">
                        {c.description}
                      </div>
                    ) : null}
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline">{AUTH_LABELS[c.auth_type]}</Badge>
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {c.auth_type === "in_cluster" ? "—" : c.api_server || "—"}
                  </TableCell>
                  <TableCell>
                    <span
                      className="text-sm"
                      title={(c.allowed_projects ?? [])
                        .map((id) => projectName.get(id) ?? id)
                        .join(", ")}
                    >
                      {(c.allowed_projects ?? []).length === 0
                        ? "all"
                        : (c.allowed_projects ?? []).length}
                    </span>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {new Date(c.created_at).toLocaleDateString()}
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() => setForm(clusterToDraft(c))}
                      aria-label={`Edit ${c.name}`}
                    >
                      <Pencil className="h-4 w-4" />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() => handleTest(c)}
                      disabled={pending}
                      aria-label={`Test connection ${c.name}`}
                      title="Test connection"
                    >
                      <PlugZap className="h-4 w-4" />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={() => handleDelete(c)}
                      disabled={pending}
                      aria-label={`Delete ${c.name}`}
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
            "data-[side=right]:w-full data-[side=right]:sm:w-[85vw] data-[side=right]:lg:w-[50vw]",
            "data-[side=right]:sm:max-w-[85vw] data-[side=right]:lg:max-w-[50vw]",
          )}
        >
          <SheetHeader>
            <SheetTitle>{form?.id ? "Edit cluster" : "New cluster"}</SheetTitle>
            <SheetDescription>
              Credentials are encrypted at rest and never echoed back. On edit,
              leave the credential blank to keep the stored one.
            </SheetDescription>
          </SheetHeader>

          {form ? (
            <ClusterForm
              form={form}
              setForm={setForm}
              projects={projects}
              pending={pending}
              onSave={saveForm}
              onCancel={() => setForm(null)}
            />
          ) : null}
        </SheetContent>
      </Sheet>
    </>
  );
}
