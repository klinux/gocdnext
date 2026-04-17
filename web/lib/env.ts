import { z } from "zod";

// Zod-validated process.env. Read once at module load; fails fast on boot if
// something is missing or malformed. Server-side only — do not import from a
// Client Component.

const schema = z.object({
  GOCDNEXT_API_URL: z.string().url().default("http://localhost:8153"),
});

export const env = schema.parse({
  GOCDNEXT_API_URL: process.env.GOCDNEXT_API_URL,
});

export type Env = z.infer<typeof schema>;
