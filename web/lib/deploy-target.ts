// Shared Zod schema for the native deploy-target form (ADR-0001). Lives
// outside the "use server" action module because a server-action file may
// only export async functions — both the Server Action and the client
// dialog import this. Mirrors the control-plane validation in
// server/internal/deploy/validate.go + domain.ValidEnvironmentName so the
// obvious mistakes are caught before the round-trip (the server re-validates
// and additionally fetches the ArgoCD Application).

import { z } from "zod";

// Same bound the pipeline parser + registrar enforce on an environment name:
// start alphanumeric, then alphanumeric + . _ - , max 64. Keeping it identical
// means the UI can't offer a name no pipeline could reference.
export const ENVIRONMENT_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

export const SYNC_MODES = ["trigger", "observe"] as const;

export const deployTargetFormSchema = z.object({
  environment: z
    .string()
    .regex(
      ENVIRONMENT_NAME_RE,
      "start with a letter or digit, then letters, digits, . _ - (max 64)",
    ),
  // The ArgoCD "hub" cluster whose k8s API hosts the Application CR — a
  // registered cluster name (not the workload's destination). Free text: the
  // clusters list is admin-gated, so a maintainer registering a target may not
  // be able to enumerate them; the server validates the name via a real
  // Application fetch and returns a clear 422 on a bad one.
  cluster: z.string().trim().min(1, "cluster is required"),
  application: z.string().trim().min(1, "application is required"),
  // Namespace holding the Application CR; server defaults empty → "argocd".
  namespace: z.string().trim().max(253).optional(),
  sync_mode: z.enum(SYNC_MODES),

  // Rollout observation (Phase 2). When on, gocdnext reads the Argo Rollouts CR the
  // Application manages and surfaces canary/blue-green progress (read-only — no
  // promote/abort control yet). Routing optional: rollout_cluster empty → the App's
  // cluster; namespace/name empty → auto-discover the single Rollout.
  rollout_aware: z.boolean().optional(),
  rollout_cluster: z.string().trim().max(64).optional(),
  rollout_namespace: z.string().trim().max(253).optional(),
  rollout_name: z.string().trim().max(253).optional(),
});

export type DeployTargetForm = z.infer<typeof deployTargetFormSchema>;
