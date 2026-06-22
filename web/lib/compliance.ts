// Compliance pipelines namespace every stage and job a policy contributes with
// this prefix (repo YAML may not use it), and name the server-owned synthetic
// pipeline `_compliance`. Mirrors server/pkg/compliance (ReservedPrefix +
// PipelineName) — kept as literals so the UI can flag enforced/managed entries
// purely from a name, no extra round-trip. The prefix surviving minor Go-side
// renames is the contract; if it changes, badges degrade to absent (never wrong).
export const COMPLIANCE_ENTRY_PREFIX = "_compliance_";
export const COMPLIANCE_PIPELINE_NAME = "_compliance";

// isComplianceEntry reports whether a stage or job name was contributed by a
// compliance policy — i.e. it is enforced and can't be removed from the repo.
export function isComplianceEntry(name: string): boolean {
  return name.startsWith(COMPLIANCE_ENTRY_PREFIX);
}

// isCompliancePipeline reports whether a pipeline is the server-owned synthetic
// compliance pipeline. Matches the Go IsReservedPipelineName (prefix match);
// repo pipelines may not use the prefix, so this never flags a user's own.
export function isCompliancePipeline(name: string): boolean {
  return name.startsWith(COMPLIANCE_PIPELINE_NAME);
}
