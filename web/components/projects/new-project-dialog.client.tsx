"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import { GitBranch, Plus, Sparkles } from "lucide-react";
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import { pipelineTemplates } from "@/lib/pipeline-templates";
import { createProject } from "@/server/actions/projects";
import { WebhookSecretDialog } from "@/components/projects/webhook-secret-dialog.client";

type Mode = "repo" | "template" | "empty";

export function NewProjectDialog() {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const [open, setOpen] = useState(false);

  // Shared metadata.
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  // Repo-relative folder for pipeline YAMLs. ".gocdnext" is the
  // default; teams with a different convention (`.woodpecker`,
  // `apps/api/.gocdnext`, …) override per project.
  const [configPath, setConfigPath] = useState(".gocdnext");

  const [mode, setMode] = useState<Mode>("repo");

  // Repo tab.
  const [scmProvider, setScmProvider] = useState<"github" | "gitlab" | "bitbucket" | "manual">("github");
  const [scmURL, setScmURL] = useState("");
  const [scmBranch, setScmBranch] = useState("main");
  const [scmSecret, setScmSecret] = useState("");

  // Template tab.
  const [templateID, setTemplateID] = useState(pipelineTemplates[0]!.id);
  const [templateYAML, setTemplateYAML] = useState(pipelineTemplates[0]!.yaml);
  const [templateTouched, setTemplateTouched] = useState(false);

  // One-shot reveal for an auto-generated webhook secret. The
  // backend returns it exactly once on /projects/apply when the
  // caller didn't supply their own; we show it modally on top of
  // the (now-closed) create dialog so the user can copy before
  // navigating away.
  const [generatedSecret, setGeneratedSecret] = useState<string | null>(null);
  const [generatedSecretOpen, setGeneratedSecretOpen] = useState(false);
  const [createdSlug, setCreatedSlug] = useState<string | null>(null);

  const reset = () => {
    setSlug("");
    setName("");
    setDescription("");
    setConfigPath(".gocdnext");
    setMode("repo");
    setScmProvider("github");
    setScmURL("");
    setScmBranch("main");
    setScmSecret("");
    setTemplateID(pipelineTemplates[0]!.id);
    setTemplateYAML(pipelineTemplates[0]!.yaml);
    setTemplateTouched(false);
  };

  const pickTemplate = (id: string) => {
    setTemplateID(id);
    const tpl = pipelineTemplates.find((t) => t.id === id);
    if (tpl && !templateTouched) {
      setTemplateYAML(tpl.yaml);
    }
  };

  const canSubmit = slug.trim().length > 0 && name.trim().length > 0 && (
    mode === "empty" ||
    (mode === "repo" && scmURL.trim().length > 0) ||
    (mode === "template" && templateYAML.trim().length > 0)
  );

  const onSubmit = () => {
    startTransition(async () => {
      const input: Parameters<typeof createProject>[0] = {
        slug: slug.trim(),
        name: name.trim(),
        description: description.trim() || undefined,
        config_path: configPath.trim() || undefined,
      };
      if (mode === "repo") {
        input.scm_source = {
          provider: scmProvider,
          url: scmURL.trim(),
          default_branch: scmBranch.trim() || "main",
          webhook_secret: scmSecret.trim() || undefined,
        };
      } else if (mode === "template") {
        const tpl = pipelineTemplates.find((t) => t.id === templateID);
        input.files = [
          { name: tpl?.filename ?? "pipeline.yml", content: templateYAML },
        ];
      }

      const res = await createProject(input);
      if (res.ok) {
        toast.success(`Project ${slug} created`, {
          action: {
            label: "Open",
            onClick: () => router.push(`/projects/${slug}` as Route),
          },
        });
        const data = res.data as {
          scm_source?: { generated_webhook_secret?: string };
          warnings?: string[];
        };
        // Surface every backend-emitted warning as its own toast —
        // the common case is "bound repo but .gocdnext/ is empty",
        // which is expected when the user creates the project
        // before pushing config. A single toast per warning reads
        // better than concatenating them into one line.
        for (const w of data?.warnings ?? []) {
          toast.warning(w, { duration: 8000 });
        }
        const scm = data?.scm_source;
        const secret = scm?.generated_webhook_secret;
        reset();
        setOpen(false);
        if (secret) {
          // Keep the user on the current page so they can copy the
          // secret before navigating to the project. The reveal
          // dialog's Done button triggers the push + refresh.
          setGeneratedSecret(secret);
          setCreatedSlug(slug);
          setGeneratedSecretOpen(true);
        } else {
          router.push(`/projects/${slug}` as Route);
          router.refresh();
        }
      } else {
        toast.error(`Create failed: ${res.error}`);
      }
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
      <Button size="sm" onClick={() => setOpen(true)}>
        <Plus className="size-3.5" /> New project
      </Button>

      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>New project</DialogTitle>
          <DialogDescription>
            Register a project and (optionally) its pipelines. You can always
            add or edit pipelines later by pushing to a connected repo or via{" "}
            <code className="font-mono">gocdnext apply</code>.
          </DialogDescription>
        </DialogHeader>

        {/* Metadata block — shared across all three paths */}
        <div className="grid gap-3 md:grid-cols-2">
          <Field label="Slug" hint="lowercase, digits, dashes">
            <Input
              value={slug}
              onChange={(e) => setSlug(e.target.value.toLowerCase())}
              placeholder="my-app"
              autoFocus
            />
          </Field>
          <Field label="Name">
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="My App"
            />
          </Field>
        </div>
        <Field label="Description (optional)">
          <Input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="What this project does, who owns it…"
          />
        </Field>
        <Field
          label="Config folder"
          hint="repo-relative path to pipeline YAMLs · default .gocdnext"
        >
          <Input
            value={configPath}
            onChange={(e) => setConfigPath(e.target.value)}
            placeholder=".gocdnext"
          />
        </Field>

        {/* Source tabs */}
        <div className="pt-1">
          <p className="mb-2 text-xs font-medium text-muted-foreground">
            How do you want to start?
          </p>
          <Tabs value={mode} onValueChange={(v) => setMode(v as Mode)}>
            <TabsList className="grid w-full grid-cols-3">
              <TabsTrigger value="repo">
                <GitBranch className="size-3.5" /> Connect repo
              </TabsTrigger>
              <TabsTrigger value="template">
                <Sparkles className="size-3.5" /> Template
              </TabsTrigger>
              <TabsTrigger value="empty">Empty</TabsTrigger>
            </TabsList>

            <TabsContent value="repo" className="space-y-3 pt-3">
              <p className="text-xs text-muted-foreground">
                Registers an SCM source. Pipelines appear once the repo pushes
                a <code className="font-mono">.gocdnext/</code> folder.
              </p>
              <div className="grid grid-cols-3 gap-3">
                <Field label="Provider">
                  <select
                    value={scmProvider}
                    onChange={(e) =>
                      setScmProvider(e.target.value as typeof scmProvider)
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
                    value={scmBranch}
                    onChange={(e) => setScmBranch(e.target.value)}
                    placeholder="main"
                  />
                </Field>
              </div>
              <Field label="Clone URL">
                <Input
                  value={scmURL}
                  onChange={(e) => setScmURL(e.target.value)}
                  placeholder="https://github.com/org/repo"
                />
              </Field>
              <Field label="Webhook secret (optional)">
                <Input
                  value={scmSecret}
                  onChange={(e) => setScmSecret(e.target.value)}
                  placeholder="same value you'll configure in the provider UI"
                />
              </Field>
            </TabsContent>

            <TabsContent value="template" className="space-y-3 pt-3">
              <p className="text-xs text-muted-foreground">
                Drops a <code className="font-mono">pipeline.yml</code> into
                the project. Edit the YAML below before creating.
              </p>
              <div className="grid gap-2">
                {pipelineTemplates.map((t) => {
                  const active = t.id === templateID;
                  return (
                    <button
                      key={t.id}
                      type="button"
                      onClick={() => pickTemplate(t.id)}
                      className={cn(
                        "flex flex-col items-start gap-0.5 rounded-md border px-3 py-2 text-left transition-colors",
                        active
                          ? "border-primary/50 bg-primary/5"
                          : "hover:border-border/80 hover:bg-muted",
                      )}
                    >
                      <span className="text-sm font-medium">{t.label}</span>
                      <span className="text-[11px] text-muted-foreground">
                        {t.description}
                      </span>
                    </button>
                  );
                })}
              </div>
              <Field label="Preview (editable)">
                <Textarea
                  value={templateYAML}
                  onChange={(e) => {
                    setTemplateYAML(e.target.value);
                    setTemplateTouched(true);
                  }}
                  className="h-44 font-mono text-xs"
                  spellCheck={false}
                />
              </Field>
            </TabsContent>

            <TabsContent value="empty" className="space-y-2 pt-3">
              <p className="text-sm">
                Creates the project with no pipelines yet.
              </p>
              <p className="text-xs text-muted-foreground">
                You can connect a repo from the project page later, or paste a
                pipeline via the Template tab on re-open.
              </p>
            </TabsContent>
          </Tabs>
        </div>

        <DialogFooter>
          <Button
            variant="ghost"
            onClick={() => setOpen(false)}
            disabled={pending}
          >
            Cancel
          </Button>
          <Button onClick={onSubmit} disabled={!canSubmit || pending}>
            {pending ? "Creating…" : "Create project"}
          </Button>
        </DialogFooter>
      </DialogContent>

      <WebhookSecretDialog
        open={generatedSecretOpen}
        secret={generatedSecret ?? ""}
        variant="create"
        title="Webhook secret generated"
        subtitle="We generated a webhook secret for this project. Register it in your provider's webhook configuration now — it won't be shown again."
        onOpenChange={(next) => {
          setGeneratedSecretOpen(next);
          if (!next) {
            const target = createdSlug;
            setGeneratedSecret(null);
            setCreatedSlug(null);
            if (target) {
              router.push(`/projects/${target}` as Route);
              router.refresh();
            }
          }
        }}
      />
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
