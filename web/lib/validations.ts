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
