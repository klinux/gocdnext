"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { Pencil } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { createProject } from "@/server/actions/projects";
import type { ProjectSCMInfo, ProjectSummary } from "@/types/api";

type Props = {
  project: ProjectSummary;
  scmSource?: ProjectSCMInfo;
};

// EditProjectDialog reuses createProject (the /apply endpoint is an
// idempotent upsert) to edit metadata + scm_source binding on an
// existing project. Slug is read-only — changing it would invalidate
// URLs, webhook configs, and agent references; operators who really
// need a new slug should delete + recreate.
//
// Pipeline files are not editable here. Definitions come from the
// repo's .gocdnext/ folder; the initial-sync-on-bind behavior pulls
// them at apply time. To change pipelines, edit the repo and push.
export function EditProjectDialog({ project, scmSource }: Props) {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [pending, startTransition] = useTransition();

  const [name, setName] = useState(project.name);
  const [description, setDescription] = useState(project.description ?? "");
  const [configPath, setConfigPath] = useState(project.config_path ?? ".gocdnext");

  const [bindRepo, setBindRepo] = useState(Boolean(scmSource));
  const [provider, setProvider] = useState<ProjectSCMInfo["provider"]>(
    scmSource?.provider ?? "github",
  );
  const [url, setUrl] = useState(scmSource?.url ?? "");
  const [branch, setBranch] = useState(scmSource?.default_branch ?? "main");
  const [authRef, setAuthRef] = useState(scmSource?.auth_ref ?? "");

  const canSubmit =
    name.trim().length > 0 &&
    (!bindRepo || url.trim().length > 0);

  const reset = () => {
    setName(project.name);
    setDescription(project.description ?? "");
    setConfigPath(project.config_path ?? ".gocdnext");
    setBindRepo(Boolean(scmSource));
    setProvider(scmSource?.provider ?? "github");
    setUrl(scmSource?.url ?? "");
    setBranch(scmSource?.default_branch ?? "main");
    setAuthRef(scmSource?.auth_ref ?? "");
  };

  const submit = () => {
    startTransition(async () => {
      const input: Parameters<typeof createProject>[0] = {
        slug: project.slug,
        name: name.trim(),
        description: description.trim() || undefined,
        config_path: configPath.trim() || undefined,
      };
      if (bindRepo) {
        input.scm_source = {
          provider,
          url: url.trim(),
          default_branch: branch.trim() || "main",
          // webhook_secret omitted on purpose — preserves the
          // sealed ciphertext already in the DB. Rotation is a
          // separate explicit action.
        };
      }

      const res = await createProject(input);
      if (!res.ok) {
        toast.error(`Save failed: ${res.error}`);
        return;
      }
      const data = res.data as { warnings?: string[] };
      for (const w of data?.warnings ?? []) {
        toast.warning(w, { duration: 8000 });
      }
      toast.success(`Project ${project.slug} updated`);
      setOpen(false);
      router.refresh();
    });
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) reset();
      }}
    >
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        <Pencil className="size-3.5" />
        Edit project
      </Button>

      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Edit {project.slug}</DialogTitle>
          <DialogDescription>
            Slug is locked after creation. Pipelines live in the repo&apos;s{" "}
            <code className="font-mono">{configPath || ".gocdnext"}</code>{" "}
            folder — edit them by pushing to the branch below.
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-3 md:grid-cols-2">
          <Field label="Slug" hint="read-only">
            <Input value={project.slug} disabled readOnly />
          </Field>
          <Field label="Name">
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </Field>
        </div>
        <Field label="Description (optional)">
          <Input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </Field>
        <Field
          label="Config folder"
          hint="repo-relative path to pipeline YAMLs"
        >
          <Input
            value={configPath}
            onChange={(e) => setConfigPath(e.target.value)}
            placeholder=".gocdnext"
          />
        </Field>

        <div className="rounded-md border p-3">
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={bindRepo}
              onChange={(e) => setBindRepo(e.target.checked)}
            />
            Repository binding
          </label>
          {bindRepo ? (
            <div className="mt-3 space-y-3">
              <div className="grid grid-cols-3 gap-3">
                <Field label="Provider">
                  <select
                    value={provider}
                    onChange={(e) =>
                      setProvider(e.target.value as ProjectSCMInfo["provider"])
                    }
                    className="h-8 w-full rounded-md border bg-background px-2 text-sm"
                  >
                    <option value="github">github</option>
                    <option value="gitlab">gitlab</option>
                    <option value="bitbucket">bitbucket</option>
                    <option value="manual">manual</option>
                  </select>
                </Field>
                <Field label="Default branch" className="col-span-2">
                  <Input
                    value={branch}
                    onChange={(e) => setBranch(e.target.value)}
                    placeholder="main"
                  />
                </Field>
              </div>
              <Field label="Clone URL">
                <Input
                  value={url}
                  onChange={(e) => setUrl(e.target.value)}
                  placeholder="https://github.com/org/repo"
                />
              </Field>
              <Field
                label="Auth ref (optional)"
                hint="PAT or credentials key — stored server-side"
              >
                <Input
                  value={authRef}
                  onChange={(e) => setAuthRef(e.target.value)}
                  placeholder="leave empty to keep existing"
                />
              </Field>
              <p className="text-xs text-muted-foreground">
                Webhook secret is not editable here — use the rotate button to
                generate a new one.
              </p>
            </div>
          ) : (
            <p className="mt-2 text-xs text-muted-foreground">
              Disabling this here leaves the binding untouched in the database
              (a future rebinding via apply still works). To clear the
              binding permanently, delete and recreate the project.
            </p>
          )}
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={() => setOpen(false)} disabled={pending}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={!canSubmit || pending}>
            {pending ? "Saving…" : "Save changes"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function Field({
  label,
  hint,
  className,
  children,
}: {
  label: string;
  hint?: string;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={cn("space-y-1", className)}>
      <Label className="text-xs text-muted-foreground">
        {label}
        {hint ? (
          <span className="ml-1 text-[10px] normal-case text-muted-foreground/70">
            · {hint}
          </span>
        ) : null}
      </Label>
      {children}
    </div>
  );
}
