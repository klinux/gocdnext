"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import {
  FolderSync,
  MoreHorizontal,
  RefreshCw,
  Trash2,
} from "lucide-react";
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { WebhookSecretDialog } from "@/components/projects/webhook-secret-dialog.client";
import { EditProjectDialog } from "@/components/projects/edit-project-dialog.client";
import {
  deleteProject,
  rotateWebhookSecret,
  syncProjectFromRepo,
} from "@/server/actions/projects";
import type { ProjectSCMInfo, ProjectSummary } from "@/types/api";

type Props = {
  project: ProjectSummary;
  scmSource?: ProjectSCMInfo;
  // Counts surfaced from the detail query so the delete confirm
  // can show blast-radius numbers without probing the server
  // twice. The backend re-counts at delete time for the success
  // toast, so slight drift between render and click is fine.
  pipelineCount: number;
  runCount: number;
};

// ProjectActionsMenu collapses five header buttons (VSM, secrets,
// edit, rotate, delete) into one dropdown plus the dialogs they
// drive. Putting the dialog state alongside the trigger keeps the
// open/close plumbing off the page.tsx and avoids controller
// refactors on each individual dialog component.
export function ProjectActionsMenu({
  project,
  scmSource,
  pipelineCount,
  runCount,
}: Props) {
  const router = useRouter();

  // Rotate flow — confirm, then one-shot reveal.
  const [rotateConfirmOpen, setRotateConfirmOpen] = useState(false);
  const [rotateRevealOpen, setRotateRevealOpen] = useState(false);
  const [rotatedSecret, setRotatedSecret] = useState("");
  const [rotating, rotateStart] = useTransition();

  const rotate = () => {
    rotateStart(async () => {
      const res = await rotateWebhookSecret(project.slug);
      if (!res.ok) {
        if (res.status === 404) {
          toast.error("No SCM source bound to this project yet");
        } else if (res.status === 503) {
          toast.error(
            "Server has no encryption key configured (GOCDNEXT_SECRET_KEY)",
          );
        } else {
          toast.error(`Rotation failed: ${res.error}`);
        }
        setRotateConfirmOpen(false);
        return;
      }
      setRotatedSecret(res.generatedWebhookSecret);
      setRotateConfirmOpen(false);
      setRotateRevealOpen(true);

      // Surface the webhook-reconcile outcome. Rotate also re-runs
      // the reconcile on the server, so the user knows whether
      // the new secret was PATCHed onto GitHub (status=updated),
      // freshly installed (registered), or still needs their
      // attention (skipped_no_install / failed).
      const w = res.webhook;
      if (w) {
        const repo = w.scm_source_url;
        switch (w.status) {
          case "registered":
            toast.success(`Webhook installed on ${repo}`, { duration: 6000 });
            break;
          case "updated":
            toast.success(`Webhook secret synced on ${repo}`, { duration: 6000 });
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
              `Webhook sync failed: ${w.error ?? "unknown error"}`,
              { duration: 10000 },
            );
            break;
        }
      }
    });
  };

  // Sync flow — one-click, no confirm dialog. The underlying
  // ApplyProject is idempotent, so an accidental click is cheap
  // (same-shape diff = zero changes). We still show the outcome
  // in a toast so the user sees what landed.
  const [syncing, syncStart] = useTransition();
  const hasSCM = Boolean(scmSource);

  const sync = () => {
    syncStart(async () => {
      const res = await syncProjectFromRepo(project.slug);
      if (!res.ok) {
        switch (res.status) {
          case 404:
            toast.error("Project not found");
            break;
          case 409:
            toast.error("No repo bound to this project yet — connect one first");
            break;
          case 502:
            toast.error(`Couldn't fetch from repo: ${res.error}`, { duration: 8000 });
            break;
          case 503:
            toast.error("Server isn't configured to fetch from SCM (no GitHub App)");
            break;
          default:
            toast.error(`Sync failed: ${res.error}`);
        }
        return;
      }
      const created = res.pipelines.filter((p) => p.created).length;
      const updated = res.pipelines.length - created;
      const removed = res.pipelinesRemoved.length;
      const parts: string[] = [];
      if (created) parts.push(`${created} created`);
      if (updated) parts.push(`${updated} updated`);
      if (removed) parts.push(`${removed} removed`);
      const summary = parts.length > 0 ? parts.join(", ") : "no changes";
      toast.success(`Synced from repo — ${summary}`, { duration: 6000 });
      for (const w of res.warnings) toast.warning(w, { duration: 8000 });
      router.refresh();
    });
  };

  // Delete flow — type-to-confirm gate.
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [typed, setTyped] = useState("");
  const [deleting, deleteStart] = useTransition();

  const deleteMatches = typed.trim() === project.slug;

  const submitDelete = () => {
    if (!deleteMatches) return;
    deleteStart(async () => {
      const res = await deleteProject(project.slug);
      if (!res.ok) {
        toast.error(`Delete failed: ${res.error}`);
        return;
      }
      const { counts } = res;
      toast.success(
        `Deleted ${project.slug} (${counts.pipelines_deleted} pipelines, ${counts.runs_deleted} runs, ${counts.secrets_deleted} secrets)`,
      );
      setDeleteOpen(false);
      setTyped("");
      router.push("/projects");
      router.refresh();
    });
  };

  return (
    <>
      <div className="flex items-center gap-2">
        <EditProjectDialog project={project} scmSource={scmSource} />
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <Button variant="outline" size="sm" aria-label="More actions">
                <MoreHorizontal className="size-3.5" />
              </Button>
            }
          />
          <DropdownMenuContent align="end" className="min-w-52">
            <DropdownMenuItem
              onClick={sync}
              disabled={!hasSCM || syncing}
              className="whitespace-nowrap"
            >
              <FolderSync className="size-3.5" />
              {syncing ? "Syncing…" : "Sync from repo"}
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => setRotateConfirmOpen(true)}
              className="whitespace-nowrap"
            >
              <RefreshCw className="size-3.5" />
              Rotate webhook secret
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              onClick={() => setDeleteOpen(true)}
              className="whitespace-nowrap text-destructive focus:text-destructive"
            >
              <Trash2 className="size-3.5" />
              Delete project
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <Dialog open={rotateConfirmOpen} onOpenChange={setRotateConfirmOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Rotate webhook secret?</DialogTitle>
            <DialogDescription>
              A new 32-byte secret will be generated and sealed in the
              database. Any existing webhook signed with the previous secret
              will start returning <code className="font-mono">401</code> until
              you update the provider configuration with the new value.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="ghost"
              onClick={() => setRotateConfirmOpen(false)}
              disabled={rotating}
            >
              Cancel
            </Button>
            <Button onClick={rotate} disabled={rotating}>
              {rotating ? "Rotating…" : "Rotate"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <WebhookSecretDialog
        open={rotateRevealOpen}
        secret={rotatedSecret}
        title="New webhook secret"
        subtitle="Copy the new value and update your provider's webhook configuration."
        onOpenChange={(next) => {
          setRotateRevealOpen(next);
          if (!next) setRotatedSecret("");
        }}
      />

      <Dialog
        open={deleteOpen}
        onOpenChange={(next) => {
          setDeleteOpen(next);
          if (!next) setTyped("");
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Delete {project.slug}?</DialogTitle>
            <DialogDescription>
              This permanently removes the project and everything under it:{" "}
              <strong>{pipelineCount}</strong> pipelines,{" "}
              <strong>{runCount}</strong> runs, every material, artifact,
              secret and SCM binding. <strong>Cannot be undone.</strong>
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label className="text-xs text-muted-foreground">
              Type <code className="font-mono">{project.slug}</code> to confirm
            </Label>
            <Input
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              placeholder={project.slug}
              autoFocus
              autoComplete="off"
            />
          </div>
          <DialogFooter>
            <Button
              variant="ghost"
              onClick={() => setDeleteOpen(false)}
              disabled={deleting}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={submitDelete}
              disabled={!deleteMatches || deleting}
            >
              {deleting ? "Deleting…" : "Delete project"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
