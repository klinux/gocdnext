"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { z } from "zod";
import { env } from "@/lib/env";

// Matches the server parser's bounds: empty disables, otherwise
// 1m <= d <= 24h. Format matches Go's time.ParseDuration so "5m",
// "1h30m", "2h" all work. The regex here is a client-friendly
// sanity check — the server re-validates authoritatively.
const durationRe = /^(\d+h)?(\d+m)?(\d+s)?$/;

const setPollIntervalSchema = z.object({
  slug: z.string().min(1),
  interval: z
    .string()
    .refine(
      (s) => s === "" || durationRe.test(s),
      "use Go duration format (e.g. 5m, 1h30m, 2h) or leave empty to disable",
    ),
});

export type ActionResult = { ok: true } | { ok: false; error: string };

export async function setProjectPollInterval(
  input: z.infer<typeof setPollIntervalSchema>,
): Promise<ActionResult> {
  const parsed = setPollIntervalSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/poll-interval`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "PUT",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ interval: parsed.data.interval }),
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 300) || "save failed"}`,
      };
    }
    revalidatePath(`/projects/${parsed.data.slug}/settings`);
    return { ok: true };
  } catch (err) {
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}

// log_archive_enabled override on the projects table. Three valid
// states: true (always archive), false (never archive), null (use
// the global default the operator set via GOCDNEXT_LOG_ARCHIVE).
const setLogArchiveSchema = z.object({
  slug: z.string().min(1),
  enabled: z.union([z.boolean(), z.null()]),
});

export async function setProjectLogArchive(
  input: z.infer<typeof setLogArchiveSchema>,
): Promise<ActionResult> {
  const parsed = setLogArchiveSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/log-archive`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "PUT",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ enabled: parsed.data.enabled }),
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 300) || "save failed"}`,
      };
    }
    revalidatePath(`/projects/${parsed.data.slug}/settings`);
    return { ok: true };
  } catch (err) {
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}

// Project labels — free-form key:value grouping tags (team:payments,
// tier:critical). The server replaces the whole set on PUT and re-validates
// (key required, bounds); this is a client-friendly pre-check.
const labelSchema = z.object({
  key: z
    .string()
    .trim()
    .min(1, "label key is required")
    .max(100)
    .refine((s) => !s.includes(":"), "label key must not contain ':'"),
  value: z.string().trim().max(100),
});
const setLabelsSchema = z.object({
  slug: z.string().min(1),
  labels: z.array(labelSchema).max(50),
});

export async function setProjectLabels(
  input: z.infer<typeof setLabelsSchema>,
): Promise<ActionResult> {
  const parsed = setLabelsSchema.safeParse(input);
  if (!parsed.success) {
    return {
      ok: false,
      error: parsed.error.issues[0]?.message ?? "invalid input",
    };
  }
  try {
    const url =
      env.GOCDNEXT_API_URL.replace(/\/+$/, "") +
      `/api/v1/projects/${encodeURIComponent(parsed.data.slug)}/labels`;
    const session = (await cookies()).get("gocdnext_session")?.value;
    const res = await fetch(url, {
      method: "PUT",
      cache: "no-store",
      headers: {
        "Content-Type": "application/json",
        ...(session ? { Cookie: `gocdnext_session=${session}` } : {}),
      },
      body: JSON.stringify({ labels: parsed.data.labels }),
    });
    if (!res.ok) {
      const body = await res.text();
      return {
        ok: false,
        error: `server ${res.status}: ${body.trim().slice(0, 300) || "save failed"}`,
      };
    }
    revalidatePath(`/projects/${parsed.data.slug}/settings`);
    revalidatePath("/projects");
    return { ok: true };
  } catch (err) {
    return {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}
