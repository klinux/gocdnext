// Pure helpers for rendering a secret's source/backend, shared by the
// RSC pages (project + admin secrets) and the client table. Kept free
// of any I/O or React so both the server and client trees can import it
// without dragging a "use client" boundary across.

import type { Secret, SecretSource } from "@/types/api";

// SOURCE_BADGE is the short label shown in a table column.
const SOURCE_BADGE: Record<SecretSource, string> = {
  db: "Stored",
  vault: "Vault",
  gcp: "GCP",
  aws: "AWS",
};

// sourceBadge returns the short backend label. Unknown sources (a
// server that grows a new backend before the UI does) fall back to
// the raw string so the column never renders blank.
export function sourceBadge(source: string): string {
  if (source in SOURCE_BADGE) return SOURCE_BADGE[source as SecretSource];
  return source;
}

// secretSourceSummary describes where a secret resolves from, for a
// table cell. For "db" it's just "Stored"; for an external backend it
// appends the ref so an operator can spot a mis-pointed secret at a
// glance: "Vault · secret/ci/github#token".
export function secretSourceSummary(secret: Secret): string {
  if (secret.source === "db" || !secret.ref) {
    return sourceBadge(secret.source);
  }
  const { path, key } = secret.ref;
  const ref = key ? `${path}#${key}` : path;
  return `${sourceBadge(secret.source)} · ${ref}`;
}
