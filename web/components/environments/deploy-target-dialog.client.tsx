"use client";

import { type ReactNode, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { Loader2, Rocket, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { deployTargetFormSchema } from "@/lib/deploy-target";
import {
  deleteDeployTarget,
  setDeployTarget,
} from "@/server/actions/environments";
import type { DeployTarget } from "@/types/api";

// base-ui Select needs an items map so the trigger shows the label, not the
// raw value (regression-guarded in select.test.tsx).
const SYNC_MODE_LABELS: Record<string, string> = {
  trigger: "trigger — gocdnext syncs after the gate",
  observe: "observe — watch an auto-synced app",
};

type Props = {
  slug: string;
  trigger: ReactNode;
  // When set, the dialog edits this existing target (fields pre-filled, the
  // environment name locked — it's the upsert key). Absent → create mode.
  initial?: DeployTarget;
  // Create mode: lock the environment name to this value (the "add a target to
  // this existing environment" affordance on a card). Ignored when `initial`
  // is set.
  presetEnvironment?: string;
};

export function DeployTargetDialog({
  slug,
  trigger,
  initial,
  presetEnvironment,
}: Props) {
  const router = useRouter();
  const isEdit = initial !== undefined;
  const envLocked = isEdit || presetEnvironment !== undefined;

  const [open, setOpen] = useState(false);
  const [environment, setEnvironment] = useState(
    initial?.environment ?? presetEnvironment ?? "",
  );
  const [cluster, setCluster] = useState(initial?.cluster ?? "");
  const [application, setApplication] = useState(initial?.application ?? "");
  const [namespace, setNamespace] = useState(initial?.namespace ?? "");
  const [syncMode, setSyncMode] = useState<"trigger" | "observe">(
    initial?.sync_mode ?? "trigger",
  );
  const [confirmingRemove, setConfirmingRemove] = useState(false);
  const [pending, startTransition] = useTransition();

  const reset = () => {
    setEnvironment(initial?.environment ?? presetEnvironment ?? "");
    setCluster(initial?.cluster ?? "");
    setApplication(initial?.application ?? "");
    setNamespace(initial?.namespace ?? "");
    setSyncMode(initial?.sync_mode ?? "trigger");
    setConfirmingRemove(false);
  };

  // Programmatic close (a successful submit/remove) doesn't fire the Dialog's
  // onOpenChange, so reset explicitly here — otherwise reopening "Register"
  // would show the previously-entered values.
  const closeAndReset = () => {
    setOpen(false);
    reset();
  };

  const submit = () => {
    const candidate = {
      environment: environment.trim(),
      cluster: cluster.trim(),
      application: application.trim(),
      namespace: namespace.trim() || undefined,
      sync_mode: syncMode,
    };
    const parsed = deployTargetFormSchema.safeParse(candidate);
    if (!parsed.success) {
      toast.error(parsed.error.issues[0]?.message ?? "invalid input");
      return;
    }
    startTransition(async () => {
      const res = await setDeployTarget({ slug, ...parsed.data });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success(
        `Native target ${isEdit ? "updated" : "registered"} for ${candidate.environment}`,
      );
      closeAndReset();
      router.refresh();
    });
  };

  const remove = () => {
    startTransition(async () => {
      const res = await deleteDeployTarget({
        slug,
        environment: environment.trim(),
      });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success(`Native target removed from ${environment.trim()}`);
      closeAndReset();
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
      <DialogTrigger render={trigger as React.ReactElement} />
      <DialogContent>
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <Rocket className="size-4" />
            {isEdit ? "Edit native deploy target" : "Register native deploy target"}
          </DialogTitle>
          <DialogDescription>
            gocdnext drives the deploy through ArgoCD — it syncs the Application
            and watches it to Synced + Healthy, with no agent. The cluster is the
            ArgoCD <strong>hub</strong> (where the Application CR lives), not the
            workload&apos;s destination.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="dt-environment">Environment</Label>
            <Input
              id="dt-environment"
              placeholder="production"
              value={environment}
              onChange={(e) => setEnvironment(e.target.value)}
              disabled={pending || envLocked}
              autoFocus={!envLocked}
            />
            {envLocked ? (
              <p className="text-xs text-muted-foreground">
                The environment is the target&apos;s key and can&apos;t change —
                remove and re-create to move it.
              </p>
            ) : null}
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="dt-cluster">Cluster (ArgoCD hub)</Label>
            <Input
              id="dt-cluster"
              placeholder="argocd-hub"
              value={cluster}
              onChange={(e) => setCluster(e.target.value)}
              disabled={pending}
              autoFocus={envLocked}
            />
            <p className="text-xs text-muted-foreground">
              A registered cluster whose API hosts the ArgoCD Application. Ask an
              admin for the name if you&apos;re unsure.
            </p>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="dt-application">ArgoCD Application</Label>
            <Input
              id="dt-application"
              placeholder="shop-prod"
              value={application}
              onChange={(e) => setApplication(e.target.value)}
              disabled={pending}
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="dt-namespace">Application namespace</Label>
            <Input
              id="dt-namespace"
              placeholder="argocd"
              value={namespace}
              onChange={(e) => setNamespace(e.target.value)}
              disabled={pending}
            />
            <p className="text-xs text-muted-foreground">
              Where the Application CR lives. Defaults to{" "}
              <span className="font-mono">argocd</span>.
            </p>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="dt-sync-mode">Sync mode</Label>
            <Select
              items={SYNC_MODE_LABELS}
              value={syncMode}
              disabled={pending}
              onValueChange={(v) => {
                if (v === "trigger" || v === "observe") setSyncMode(v);
              }}
            >
              <SelectTrigger id="dt-sync-mode" aria-label="Sync mode" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="trigger">
                  trigger — gocdnext syncs after the gate
                </SelectItem>
                <SelectItem value="observe">
                  observe — watch an auto-synced app
                </SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>

        <DialogFooter className="sm:justify-between">
          {isEdit ? (
            confirmingRemove ? (
              <span className="flex items-center gap-2 text-xs">
                <span className="text-muted-foreground">Remove target?</span>
                <Button
                  variant="destructive"
                  size="sm"
                  onClick={remove}
                  disabled={pending}
                >
                  {pending ? (
                    <Loader2 className="mr-1 size-3.5 animate-spin" />
                  ) : null}
                  Confirm
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setConfirmingRemove(false)}
                  disabled={pending}
                >
                  Cancel
                </Button>
              </span>
            ) : (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="text-destructive hover:text-destructive"
                onClick={() => setConfirmingRemove(true)}
                disabled={pending}
              >
                <Trash2 className="mr-1 size-4" /> Remove
              </Button>
            )
          ) : (
            <span />
          )}
          <Button
            onClick={submit}
            disabled={
              pending ||
              !environment.trim() ||
              !cluster.trim() ||
              !application.trim()
            }
          >
            {pending ? <Loader2 className="mr-2 size-4 animate-spin" /> : null}
            {isEdit ? "Save" : "Register"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
