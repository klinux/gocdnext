"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import { GitBranch, Plus, Sparkles } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import { pipelineTemplates } from "@/lib/pipeline-templates";
import { createProject } from "@/server/actions/projects";
import { WebhookSecretDialog } from "@/components/projects/webhook-secret-dialog.client";

type Mode = "repo" | "template" | "empty";

// slugify produces the kebab-case form of a free-text project
// name. Strips accents + non-word chars, collapses whitespace,
// lowercases. Runs on every `name` keystroke unless the user has
// explicitly edited the slug — that flips slugTouched and the
// form leaves it alone thereafter.
function slugify(s: string): string {
  return s
    .toLowerCase()
    .normalize("NFKD")
    .replace(/[̀-ͯ]/g, "")
    .replace(/[^a-z0-9\s-]/g, "")
    .trim()
    .replace(/\s+/g, "-")
    .replace(/-+/g, "-");
}

export function NewProjectDialog() {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const [open, setOpen] = useState(false);

  // Shared metadata.
  const [slug, setSlug] = useState("");
  const [slugTouched, setSlugTouched] = useState(false);
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
    setSlugTouched(false);
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

  const onNameChange = (value: string) => {
    setName(value);
    // Auto-slugify until the user takes over the slug field. Once
    // they've edited it directly, respect that — same pattern
    // GitHub / GitLab use on create-repo / create-project flows.
    if (!slugTouched) {
      setSlug(slugify(value));
    }
  };

  const onSlugChange = (value: string) => {
    setSlugTouched(true);
    setSlug(value.toLowerCase());
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
          webhooks?: Array<{
            scm_source_url: string;
            status: string;
            hook_id?: number;
            error?: string;
          }>;
        };
        for (const w of data?.warnings ?? []) {
          toast.warning(w, { duration: 8000 });
        }
        for (const w of data?.webhooks ?? []) {
          const repo = w.scm_source_url;
          switch (w.status) {
            case "registered":
              toast.success(`Webhook installed on ${repo}`, { duration: 6000 });
              break;
            case "already_exists":
              toast.info(`Webhook already installed on ${repo}`, { duration: 5000 });
              break;
            case "skipped_no_install":
              toast.warning(
                `GitHub App not installed on ${repo} — install it to enable push triggers`,
                { duration: 10000 },
              );
              break;
            case "skipped_not_github":
              break;
            case "failed":
              toast.error(
                `Webhook registration failed: ${w.error ?? "unknown error"}`,
                { duration: 10000 },
              );
              break;
          }
        }
        const scm = data?.scm_source;
        const secret = scm?.generated_webhook_secret;
        reset();
        setOpen(false);
        if (secret) {
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
    <Sheet
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) reset();
      }}
    >
      <Button size="sm" onClick={() => setOpen(true)}>
        <Plus className="size-3.5" /> New project
      </Button>

      <SheetContent
        side="right"
        // Half-viewport on desktop, full-ish on narrow screens —
        // the form packs 4-5 fields + the template textarea, which
        // never fit comfortably in the 600px Dialog.
        className="flex w-full flex-col gap-0 p-0 sm:w-[min(720px,50vw)] sm:max-w-[min(720px,50vw)]"
      >
        <SheetHeader className="border-b p-6">
          <SheetTitle>New project</SheetTitle>
          <SheetDescription>
            Register a project and (optionally) its pipelines. You can always
            add or edit pipelines later by pushing to a connected repo or via{" "}
            <code className="font-mono">gocdnext apply</code>.
          </SheetDescription>
        </SheetHeader>

        <div className="flex-1 space-y-4 overflow-y-auto px-6 py-4">
          {/* Metadata block — shared across all three paths */}
          <Field label="Name">
            <Input
              value={name}
              onValueChange={onNameChange}
              placeholder="My App"
              autoFocus
            />
          </Field>
          <Field
            label="Slug"
            hint={slugTouched ? "custom — typing resets auto-sync" : "auto from name"}
          >
            <Input
              value={slug}
              onValueChange={onSlugChange}
              placeholder="my-app"
              className="font-mono"
            />
          </Field>
          <Field label="Description (optional)">
            <Input
              value={description}
              onValueChange={setDescription}
              placeholder="What this project does, who owns it…"
            />
          </Field>
          <Field
            label="Config path"
            hint="folder (.gocdnext) or single file (.gocdnext.yml)"
          >
            <Input
              value={configPath}
              onValueChange={setConfigPath}
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
                      onValueChange={setScmBranch}
                      placeholder="main"
                    />
                  </Field>
                </div>
                <Field label="Clone URL">
                  <Input
                    value={scmURL}
                    onValueChange={setScmURL}
                    placeholder="https://github.com/org/repo"
                  />
                </Field>
                <Field label="Webhook secret (optional)">
                  <Input
                    value={scmSecret}
                    onValueChange={setScmSecret}
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
        </div>

        <SheetFooter className="border-t p-6 sm:flex-row sm:justify-end">
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
        </SheetFooter>
      </SheetContent>

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
    </Sheet>
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
