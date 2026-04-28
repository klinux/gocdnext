"use client";

import { useMemo, useState, useTransition } from "react";
import { Loader2, Pencil, Plus, Search, Trash2, X } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
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
  createRunnerProfile,
  deleteRunnerProfile,
  updateRunnerProfile,
} from "@/server/actions/runner-profiles";
import type { AdminRunnerProfile } from "@/server/queries/admin";

type Props = {
  initial: AdminRunnerProfile[];
};

type FormDraft = {
  id: string | null;
  name: string;
  description: string;
  engine: "kubernetes";
  default_image: string;
  default_cpu_request: string;
  default_cpu_limit: string;
  default_mem_request: string;
  default_mem_limit: string;
  max_cpu: string;
  max_mem: string;
  tagsRaw: string; // comma-separated; parsed on save
};

function blankForm(): FormDraft {
  return {
    id: null,
    name: "",
    description: "",
    engine: "kubernetes",
    default_image: "",
    default_cpu_request: "",
    default_cpu_limit: "",
    default_mem_request: "",
    default_mem_limit: "",
    max_cpu: "",
    max_mem: "",
    tagsRaw: "",
  };
}

function profileToDraft(p: AdminRunnerProfile): FormDraft {
  return {
    id: p.id,
    name: p.name,
    description: p.description,
    engine: "kubernetes",
    default_image: p.default_image,
    default_cpu_request: p.default_cpu_request,
    default_cpu_limit: p.default_cpu_limit,
    default_mem_request: p.default_mem_request,
    default_mem_limit: p.default_mem_limit,
    max_cpu: p.max_cpu,
    max_mem: p.max_mem,
    tagsRaw: p.tags.join(", "),
  };
}

export function ProfilesManager({ initial }: Props) {
  const [profiles, setProfiles] = useState<AdminRunnerProfile[]>(initial);
  const [filter, setFilter] = useState("");
  const [form, setForm] = useState<FormDraft | null>(null);
  const [pending, startTransition] = useTransition();

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return profiles;
    return profiles.filter(
      (p) =>
        p.name.toLowerCase().includes(q) ||
        p.description.toLowerCase().includes(q) ||
        p.tags.some((t) => t.toLowerCase().includes(q)),
    );
  }, [profiles, filter]);

  const parseTags = (raw: string): string[] =>
    raw
      .split(",")
      .map((t) => t.trim())
      .filter((t) => t.length > 0);

  const saveForm = () => {
    if (!form) return;
    const name = form.name.trim();
    if (!name) {
      toast.error("Name is required");
      return;
    }
    startTransition(async () => {
      const body = {
        name,
        description: form.description,
        engine: form.engine,
        default_image: form.default_image,
        default_cpu_request: form.default_cpu_request,
        default_cpu_limit: form.default_cpu_limit,
        default_mem_request: form.default_mem_request,
        default_mem_limit: form.default_mem_limit,
        max_cpu: form.max_cpu,
        max_mem: form.max_mem,
        tags: parseTags(form.tagsRaw),
      };
      const res = form.id
        ? await updateRunnerProfile({ ...body, id: form.id })
        : await createRunnerProfile(body);
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success(form.id ? "Profile updated" : "Profile created");
      setForm(null);
      // Optimistic local update; the next server render replaces
      // this with the canonical row including timestamps.
      const draft: AdminRunnerProfile = {
        id: form.id ?? "__opt__" + Date.now(),
        name,
        description: form.description,
        engine: form.engine,
        default_image: form.default_image,
        default_cpu_request: form.default_cpu_request,
        default_cpu_limit: form.default_cpu_limit,
        default_mem_request: form.default_mem_request,
        default_mem_limit: form.default_mem_limit,
        max_cpu: form.max_cpu,
        max_mem: form.max_mem,
        tags: parseTags(form.tagsRaw),
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
      };
      setProfiles((prev) => {
        if (form.id) {
          return prev.map((p) => (p.id === form.id ? draft : p));
        }
        return [...prev, draft].sort((a, b) => a.name.localeCompare(b.name));
      });
    });
  };

  const handleDelete = (p: AdminRunnerProfile) => {
    if (!confirm(`Delete profile "${p.name}"? Pipelines referencing it will fail to apply until rewired.`)) {
      return;
    }
    startTransition(async () => {
      const res = await deleteRunnerProfile({ id: p.id });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success("Profile deleted");
      setProfiles((prev) => prev.filter((x) => x.id !== p.id));
    });
  };

  return (
    <>
      <div className="flex items-center gap-2">
        <div className="relative flex-1 max-w-sm">
          <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
          <Input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter profiles…"
            className="pl-8"
          />
        </div>
        <Button onClick={() => setForm(blankForm())}>
          <Plus className="mr-2 h-4 w-4" /> New profile
        </Button>
      </div>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Engine</TableHead>
            <TableHead>Default image</TableHead>
            <TableHead>Cap (cpu / mem)</TableHead>
            <TableHead>Tags</TableHead>
            <TableHead className="w-[120px]" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {filtered.length === 0 ? (
            <TableRow>
              <TableCell colSpan={6} className="text-center text-sm text-muted-foreground py-8">
                {profiles.length === 0
                  ? "No runner profiles yet — create one above."
                  : "No profiles match the filter."}
              </TableCell>
            </TableRow>
          ) : (
            filtered.map((p) => (
              <TableRow key={p.id}>
                <TableCell className="font-medium">
                  {p.name}
                  {p.description ? (
                    <div className="text-xs text-muted-foreground">{p.description}</div>
                  ) : null}
                </TableCell>
                <TableCell>
                  <Badge variant="outline">{p.engine}</Badge>
                </TableCell>
                <TableCell className="font-mono text-xs">{p.default_image || "—"}</TableCell>
                <TableCell className="font-mono text-xs">
                  {p.max_cpu || "—"} / {p.max_mem || "—"}
                </TableCell>
                <TableCell>
                  <div className="flex flex-wrap gap-1">
                    {p.tags.length === 0 ? (
                      <span className="text-xs text-muted-foreground">—</span>
                    ) : (
                      p.tags.map((t) => (
                        <Badge key={t} variant="secondary">
                          {t}
                        </Badge>
                      ))
                    )}
                  </div>
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    variant="ghost"
                    size="icon"
                    onClick={() => setForm(profileToDraft(p))}
                    aria-label={`Edit ${p.name}`}
                  >
                    <Pencil className="h-4 w-4" />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    onClick={() => handleDelete(p)}
                    disabled={pending}
                    aria-label={`Delete ${p.name}`}
                  >
                    <Trash2 className="h-4 w-4" />
                  </Button>
                </TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>

      <Sheet open={form !== null} onOpenChange={(open) => !open && setForm(null)}>
        <SheetContent className="sm:max-w-lg overflow-y-auto">
          <SheetHeader>
            <SheetTitle>{form?.id ? "Edit profile" : "New profile"}</SheetTitle>
            <SheetDescription>
              Profiles bundle a fallback image, default + max compute, and a
              tag set. Jobs reference them via{" "}
              <code className="rounded bg-muted px-1 py-0.5 text-xs">agent.profile</code>{" "}
              in YAML.
            </SheetDescription>
          </SheetHeader>

          {form ? (
            <div className="grid gap-4 px-6 pb-6">
              <Field label="Name" required>
                <Input
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="default"
                  autoFocus
                  disabled={!!form.id}
                />
              </Field>
              <Field label="Description">
                <Input
                  value={form.description}
                  onChange={(e) => setForm({ ...form, description: e.target.value })}
                  placeholder="What this profile is for"
                />
              </Field>
              <Field label="Default image">
                <Input
                  value={form.default_image}
                  onChange={(e) => setForm({ ...form, default_image: e.target.value })}
                  placeholder="alpine:3.20"
                />
              </Field>
              <div className="grid grid-cols-2 gap-3">
                <Field label="Default CPU req">
                  <Input
                    value={form.default_cpu_request}
                    onChange={(e) => setForm({ ...form, default_cpu_request: e.target.value })}
                    placeholder="100m"
                  />
                </Field>
                <Field label="Default CPU limit">
                  <Input
                    value={form.default_cpu_limit}
                    onChange={(e) => setForm({ ...form, default_cpu_limit: e.target.value })}
                    placeholder="1"
                  />
                </Field>
                <Field label="Default memory req">
                  <Input
                    value={form.default_mem_request}
                    onChange={(e) => setForm({ ...form, default_mem_request: e.target.value })}
                    placeholder="256Mi"
                  />
                </Field>
                <Field label="Default memory limit">
                  <Input
                    value={form.default_mem_limit}
                    onChange={(e) => setForm({ ...form, default_mem_limit: e.target.value })}
                    placeholder="1Gi"
                  />
                </Field>
                <Field label="Max CPU">
                  <Input
                    value={form.max_cpu}
                    onChange={(e) => setForm({ ...form, max_cpu: e.target.value })}
                    placeholder="4"
                  />
                </Field>
                <Field label="Max memory">
                  <Input
                    value={form.max_mem}
                    onChange={(e) => setForm({ ...form, max_mem: e.target.value })}
                    placeholder="8Gi"
                  />
                </Field>
              </div>
              <Field
                label="Tags (comma-separated)"
                hint="Required tags any agent must carry to run jobs bound to this profile. Merged with job-level agent.tags at apply time."
              >
                <Input
                  value={form.tagsRaw}
                  onChange={(e) => setForm({ ...form, tagsRaw: e.target.value })}
                  placeholder="linux, gpu"
                />
              </Field>

              <div className="mt-2 flex items-center justify-end gap-2">
                <Button variant="ghost" onClick={() => setForm(null)} disabled={pending}>
                  <X className="mr-2 h-4 w-4" /> Cancel
                </Button>
                <Button onClick={saveForm} disabled={pending}>
                  {pending ? (
                    <>
                      <Loader2 className="mr-2 h-4 w-4 animate-spin" /> Saving…
                    </>
                  ) : (
                    "Save"
                  )}
                </Button>
              </div>
            </div>
          ) : null}
        </SheetContent>
      </Sheet>
    </>
  );
}

function Field({
  label,
  hint,
  required,
  children,
}: {
  label: string;
  hint?: string;
  required?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className="grid gap-1.5">
      <Label>
        {label}
        {required ? <span className="text-destructive"> *</span> : null}
      </Label>
      {children}
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}
