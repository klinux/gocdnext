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
