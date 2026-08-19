// Shared phrasing for the environment-freeze "approval on hold" state (#227),
// used by the run-detail approve dialog and the project-flow approve menu so the
// wording never drifts between surfaces.

import type { JobDetail, StageDetail } from "@/types/api";

// freezeReason is the COMPACT sentence (collapses many envs to a count). Render
// the full `frozenEnvs` list separately (comma-joined, wrapped) when every name
// must stay accessible — this helper deliberately does not enumerate them.
export function freezeReason(frozenEnvs?: string[]): string {
  const envs = frozenEnvs ?? [];
  const subject =
    envs.length === 1
      ? `${envs.join(", ")} is`
      : envs.length > 1
        ? `${envs.length} environments are`
        : "an environment is";
  return `Approval is paused while ${subject} frozen — it resumes when unfrozen.`;
}

// `freezing` is a presentation state, not a persisted scheduler transition.
// The database remains authoritative with awaiting_approval/running while the
// live freeze annotation explains why neither node can advance.
export function jobDisplayStatus(job: JobDetail): string {
  return job.status === "awaiting_approval" &&
    job.approval_gate === true &&
    job.held_by_freeze === true
    ? "freezing"
    : job.status;
}

export function stageDisplayStatus(stage: StageDetail): string {
  if (stage.status !== "running") return stage.status;
  return stage.jobs.some((job) => jobDisplayStatus(job) === "freezing")
    ? "freezing"
    : stage.status;
}
