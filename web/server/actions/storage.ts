"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";

import { env } from "@/lib/env";

// Same per-backend contract the server enforces. Validating here
// avoids a server round-trip for the obvious typos. Keep in sync
// with admin/storage.go::validateStorageWrite.
const baseSchema = z.object({
  backend: z.enum(["filesystem", "s3", "gcs"]),
  value: z.record(z.string(), z.unknown()).default({}),
  credentials: z.record(z.string(), z.string()).default({}),
});

const s3Schema = baseSchema.extend({
  backend: z.literal("s3"),
  value: z
    .object({
      bucket: z.string().min(1, "bucket is required"),
      region: z.string().optional().default(""),
      endpoint: z.string().optional().default(""),
      use_path_style: z.boolean().optional().default(false),
      ensure_bucket: z.boolean().optional().default(false),
    })
    .passthrough(),
});

const gcsSchema = baseSchema.extend({
  backend: z.literal("gcs"),
  value: z
    .object({
      bucket: z.string().min(1, "bucket is required"),
      project_id: z.string().optional().default(""),
      ensure_bucket: z.boolean().optional().default(false),
    })
    .passthrough(),
});

const fsSchema = baseSchema.extend({
  backend: z.literal("filesystem"),
});

const writeSchema = z.discriminatedUnion("backend", [
  fsSchema,
  s3Schema,
  gcsSchema,
]);

export type ActionResult<T = void> =
  | { ok: true; data: T }
  | { ok: false; error: string };

export type SaveStorageResult = {
  restart_required: boolean;
};

async function call<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<ActionResult<T> & { restartRequired?: boolean }> {
  try {
    const url = env.GOCDNEXT_API_URL.replace(/\/+$/, "") + path;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method,
      cache: "no-store",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
    if (!res.ok) {
      const text = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${text.trim().slice(0, 300)}`,
      };
    }
    const restartRequired =
      res.headers.get("X-Gocdnext-Restart-Required") === "true";
    if (res.status === 204) {
      return { ok: true, data: undefined as T, restartRequired };
    }
    const data = (await res.json()) as T;
    return { ok: true, data, restartRequired };
  } catch (err) {
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}

export async function saveStorageConfig(
  input: z.infer<typeof writeSchema>,
): Promise<ActionResult<SaveStorageResult>> {
  const parsed = writeSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  const res = await call<unknown>("PUT", "/api/v1/admin/storage", parsed.data);
  if (!res.ok) return res;
  revalidatePath("/admin/storage");
  revalidatePath("/admin/audit");
  return {
    ok: true,
    data: { restart_required: res.restartRequired ?? false },
  };
}

export async function clearStorageConfig(): Promise<ActionResult<SaveStorageResult>> {
  const res = await call<unknown>("DELETE", "/api/v1/admin/storage");
  if (!res.ok) return res;
  revalidatePath("/admin/storage");
  revalidatePath("/admin/audit");
  return {
    ok: true,
    data: { restart_required: res.restartRequired ?? false },
  };
}
