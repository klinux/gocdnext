// Shared Zod schemas. Lives outside any "use server" file because
// Next.js only allows server-action files to export async
// functions — exporting a Zod object from one of those trips
// "A 'use server' file can only export async functions, found
// object." and breaks the whole server-action graph at build
// time. Both the server actions and the client forms import from
// here.

import { z } from "zod";

// Matches store.ValidateSecretName on the server side: letters,
// digits, underscore; must start with a letter; <=64 chars.
export const secretNameSchema = z
  .string()
  .min(1, "name is required")
  .max(64)
  .regex(
    /^[A-Za-z][A-Za-z0-9_]*$/,
    "use letters, digits and underscore; must start with a letter",
  );

// Matches projectsapi.validateSlug on the server side: lowercase
// slug with dashes, must start with a letter.
export const projectSlugSchema = z
  .string()
  .min(1, "slug is required")
  .max(64)
  .regex(
    /^[a-z][a-z0-9-]*$/,
    "lowercase letters, digits and dashes; must start with a letter",
  );

// secretValueSchema mirrors the server's 64 KiB cap on stored values.
export const secretValueSchema = z
  .string()
  .min(1, "value cannot be empty")
  .max(64 * 1024);

// secretPayloadSchema is the discriminated union the set actions accept,
// keyed on `source`. The DB variant carries an inline value; external
// variants carry a backend `ref` and never a value. Vault additionally
// requires `ref.key` because a Vault path holds a map of keys. The
// server re-validates this shape and returns a 400 on a bad combination
// (or an unconfigured source) — the client schema just keeps the obvious
// mistakes out of the round-trip.
const dbVariant = z.object({
  source: z.literal("db"),
  name: secretNameSchema,
  value: secretValueSchema,
});

const externalRef = z.object({
  path: z.string().min(1, "path is required").max(1024),
  key: z.string().max(256).optional(),
});

const externalRefWithKey = externalRef.extend({
  key: z.string().min(1, "key is required for Vault").max(256),
});

const vaultVariant = z.object({
  source: z.literal("vault"),
  name: secretNameSchema,
  ref: externalRefWithKey,
});

const gcpVariant = z.object({
  source: z.literal("gcp"),
  name: secretNameSchema,
  ref: externalRef,
});

const awsVariant = z.object({
  source: z.literal("aws"),
  name: secretNameSchema,
  ref: externalRef,
});

export const secretPayloadSchema = z.discriminatedUnion("source", [
  dbVariant,
  vaultVariant,
  gcpVariant,
  awsVariant,
]);

export type SecretPayload = z.infer<typeof secretPayloadSchema>;

// secretBackendSources is the admin-configurable backend set (db is
// in-app, so it has no connection to configure).
export const secretBackendSources = ["vault", "gcp", "aws"] as const;

// Sentinel the server understands on PUT to mean "keep the stored
// credential — the admin left the write-only field blank". Mirrors the
// cluster CREDENTIAL_PRESERVE_SENTINEL so the never-echo-plaintext rule
// holds: a credential is typed on first save / rotate, never read back.
// Here we model it as the explicit `preserve_credentials` flag the
// server accepts, so no magic string travels the wire for secrets.

// secretBackendWriteSchema validates the PUT body before the
// round-trip. `value` carries only NON-secret connection config and
// `credentials` only the write-only Vault keys (secret_id/token).
// The per-source required-field rules (vault needs addr; approle needs
// role_id; gcp needs project; aws needs region) are enforced with a
// superRefine so a single schema covers all three backends — the server
// re-validates and returns a 400 on any combination this misses. A
// discriminated union would be overkill for three near-identical shapes.
export const secretBackendWriteSchema = z
  .object({
    source: z.enum(secretBackendSources),
    enabled: z.boolean(),
    value: z.record(z.string(), z.unknown()).default({}),
    // credentials is omitted entirely for gcp/aws (ambient ADC/IRSA)
    // and on an edit that preserves the stored Vault credential.
    credentials: z.record(z.string(), z.string()).optional(),
    // preserve_credentials:true keeps the stored credential — sent when
    // editing Vault and the credential field was left blank.
    preserve_credentials: z.boolean().optional(),
  })
  .superRefine((val, ctx) => {
    if (!val.enabled) return; // disabled backends skip field checks
    const v = val.value;
    const str = (k: string): string =>
      typeof v[k] === "string" ? (v[k] as string).trim() : "";
    if (val.source === "vault") {
      if (!str("addr")) {
        ctx.addIssue({
          code: "custom",
          path: ["value", "addr"],
          message: "Vault address is required",
        });
      }
      // approle is the default auth method; it needs a role_id (the
      // matching secret_id rides in credentials, write-only).
      const auth = str("auth") || "approle";
      if (auth === "approle" && !str("role_id")) {
        ctx.addIssue({
          code: "custom",
          path: ["value", "role_id"],
          message: "AppRole role_id is required",
        });
      }
    } else if (val.source === "gcp") {
      if (!str("project")) {
        ctx.addIssue({
          code: "custom",
          path: ["value", "project"],
          message: "GCP project is required",
        });
      }
    } else if (val.source === "aws") {
      if (!str("region")) {
        ctx.addIssue({
          code: "custom",
          path: ["value", "region"],
          message: "AWS region is required",
        });
      }
    }
  });

export type SecretBackendWrite = z.infer<typeof secretBackendWriteSchema>;
