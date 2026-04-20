"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

// Server-side Zod validation. Mirrors the backend's contract —
// backend re-validates everything, so these checks are just for
// fast failure on obvious user mistakes before the round-trip.

export const projectSlugSchema = z
  .string()
  .min(1, "slug is required")
  .max(64)
  .regex(
    /^[a-z][a-z0-9-]*$/,
    "lowercase letters, digits and dashes; must start with a letter",
  );

const fileSchema = z.object({
  name: z.string().min(1),
  content: z.string().min(1),
});

const scmSourceSchema = z.object({
  provider: z.enum(["github", "gitlab", "bitbucket", "manual"]),
  url: z.string().url(),
  default_branch: z.string().min(1).optional(),
  webhook_secret: z.string().max(256).optional(),
});

const createProjectSchema = z.object({
  slug: projectSlugSchema,
  name: z.string().min(1).max(128),
  description: z.string().max(2000).optional(),
  files: z.array(fileSchema).optional(),
  scm_source: scmSourceSchema.optional(),
});

export type CreateProjectInput = z.infer<typeof createProjectSchema>;

export type CreateProjectResult =
  | { ok: true; data: Record<string, unknown> }
  | { ok: false; error: string; status?: number };

// createProject POSTs /api/v1/projects/apply with whatever subset
// the user configured in the dialog. `files` is optional — an empty
// project just registers metadata + scm_source. The API handles
// the slug-already-taken case with a 409.
export async function createProject(
  input: CreateProjectInput,
): Promise<CreateProjectResult> {
  const parsed = createProjectSchema.safeParse(input);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid input" };
  }

  const url =
    env.GOCDNEXT_API_URL.replace(/\/+$/, "") + "/api/v1/projects/apply";
  const store = await cookies();
  const session = store.get("gocdnext_session")?.value;

  try {
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({
        slug: parsed.data.slug,
        name: parsed.data.name,
        description: parsed.data.description ?? "",
        files: parsed.data.files ?? [],
        ...(parsed.data.scm_source ? { scm_source: parsed.data.scm_source } : {}),
      }),
    });
    const text = await res.text();
    if (!res.ok) {
      return {
        ok: false,
        status: res.status,
        error: text.trim().slice(0, 300) || `server ${res.status}`,
      };
    }
    revalidatePath("/projects");
    revalidatePath(`/projects/${parsed.data.slug}`);
    const data = text ? (JSON.parse(text) as Record<string, unknown>) : {};
    return { ok: true, data };
  } catch (err) {
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}
