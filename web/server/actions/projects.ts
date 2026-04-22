"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";
import { projectSlugSchema } from "@/lib/validations";

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

// Config path shape: same rule as the backend — letters, digits,
// . _ - / only, no leading slash, no "..". Empty means "default
// .gocdnext on insert / keep existing on update".
const configPathSchema = z
  .string()
  .max(512)
  .regex(
    /^(?!.*(^|\/)\.\.($|\/))[a-zA-Z0-9._-]+(\/[a-zA-Z0-9._-]+)*$/,
    "letters, digits, . _ - / only; no .., no leading /",
  )
  .optional();

const createProjectSchema = z.object({
  slug: projectSlugSchema,
  name: z.string().min(1).max(128),
  description: z.string().max(2000).optional(),
  config_path: configPathSchema,
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
        config_path: parsed.data.config_path ?? "",
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

export type DeleteProjectCounts = {
  pipelines_deleted: number;
  runs_deleted: number;
  secrets_deleted: number;
  scm_sources_deleted: number;
};

export type DeleteProjectResult =
  | { ok: true; slug: string; counts: DeleteProjectCounts }
  | { ok: false; error: string; status?: number };

// deleteProject removes the project and all cascaded children
// (pipelines, materials, runs, artifacts, secrets, scm_sources)
// via the backend's ON DELETE CASCADE wiring. Returns the
// pre-delete row counts so the UI can confirm the blast radius
// in its success toast.
export async function deleteProject(slug: string): Promise<DeleteProjectResult> {
  const parsed = projectSlugSchema.safeParse(slug);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid slug" };
  }

  const url =
    env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
    `/api/v1/projects/${encodeURIComponent(parsed.data)}`;
  const store = await cookies();
  const session = store.get("gocdnext_session")?.value;

  try {
    const res = await fetch(url, {
      method: "DELETE",
      cache: "no-store",
      headers: {
        Accept: "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
    });
    const text = await res.text();
    if (!res.ok) {
      return {
        ok: false,
        status: res.status,
        error: text.trim().slice(0, 300) || `server ${res.status}`,
      };
    }
    const body = text
      ? (JSON.parse(text) as Partial<DeleteProjectCounts> & { slug?: string })
      : {};
    revalidatePath("/projects");
    revalidatePath(`/projects/${parsed.data}`);
    return {
      ok: true,
      slug: body.slug ?? parsed.data,
      counts: {
        pipelines_deleted: body.pipelines_deleted ?? 0,
        runs_deleted: body.runs_deleted ?? 0,
        secrets_deleted: body.secrets_deleted ?? 0,
        scm_sources_deleted: body.scm_sources_deleted ?? 0,
      },
    };
  } catch (err) {
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}

export type RotateWebhookOutcome = {
  scm_source_url: string;
  status: string;
  hook_id?: number;
  error?: string;
};

export type RotateWebhookSecretResult =
  | {
      ok: true;
      scmSourceID: string;
      generatedWebhookSecret: string;
      // Present when the server also re-ran the webhook reconcile
      // (e.g. PATCHed the hook's secret on GitHub). Absent for
      // projects whose scm_source isn't a github provider or when
      // the GitHub App isn't wired.
      webhook?: RotateWebhookOutcome;
    }
  | { ok: false; error: string; status?: number };

// rotateWebhookSecret POSTs the per-project rotation endpoint. The
// plaintext secret lives only in the return value — subsequent reads
// cannot recover it, same contract the backend enforces.
export async function rotateWebhookSecret(
  slug: string,
): Promise<RotateWebhookSecretResult> {
  const parsed = projectSlugSchema.safeParse(slug);
  if (!parsed.success) {
    return { ok: false, error: parsed.error.issues[0]?.message ?? "invalid slug" };
  }

  const url =
    env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
    `/api/v1/projects/${encodeURIComponent(parsed.data)}/scm-sources/rotate-webhook-secret`;
  const store = await cookies();
  const session = store.get("gocdnext_session")?.value;

  try {
    const res = await fetch(url, {
      method: "POST",
      cache: "no-store",
      headers: {
        Accept: "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
    });
    const text = await res.text();
    if (!res.ok) {
      return {
        ok: false,
        status: res.status,
        error: text.trim().slice(0, 300) || `server ${res.status}`,
      };
    }
    const body = text
      ? (JSON.parse(text) as {
          scm_source_id?: string;
          generated_webhook_secret?: string;
          webhook?: RotateWebhookOutcome;
        })
      : {};
    if (!body.scm_source_id || !body.generated_webhook_secret) {
      return { ok: false, error: "server returned no secret" };
    }
    revalidatePath(`/projects/${parsed.data}`);
    return {
      ok: true,
      scmSourceID: body.scm_source_id,
      generatedWebhookSecret: body.generated_webhook_secret,
      webhook: body.webhook,
    };
  } catch (err) {
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}
