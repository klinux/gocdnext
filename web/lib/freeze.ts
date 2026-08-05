// Shared phrasing for the environment-freeze "approval on hold" state (#227),
// used by the run-detail approve dialog and the project-flow approve menu so the
// wording never drifts between surfaces.

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
