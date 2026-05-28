import { z } from "zod";

// Zod-validated process.env. Read once at module load; fails fast on boot if
// something is missing or malformed. Server-side only — do not import from a
// Client Component.
//
// Two URL knobs, on purpose:
//
//   GOCDNEXT_API_URL        — used by SSR fetches inside the web pod. Should
//                             point at the in-cluster control-plane service
//                             (e.g. http://gocdnext-server:8080). Always
//                             required.
//
//   GOCDNEXT_PUBLIC_API_URL — passed to Client Components for browser-side
//                             fetches. Optional: empty / unset means the
//                             browser uses RELATIVE paths (`/api/v1/...`),
//                             which works when the same ingress routes both
//                             the UI and the API. Set it explicitly only for
//                             cross-origin dev (web on :3000, server on
//                             :8153) or split-domain deployments.

const schema = z.object({
  GOCDNEXT_API_URL: z.string().url().default("http://localhost:8153"),
  GOCDNEXT_PUBLIC_API_URL: z
    .string()
    .url()
    .or(z.literal(""))
    .default(""),
});

export const env = schema.parse({
  GOCDNEXT_API_URL: process.env.GOCDNEXT_API_URL,
  GOCDNEXT_PUBLIC_API_URL: process.env.GOCDNEXT_PUBLIC_API_URL,
});

export type Env = z.infer<typeof schema>;
