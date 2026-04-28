// Astro v5's content layer requires an explicit collection
// definition; Starlight 0.30 ships a `docsLoader()` helper that
// wires the legacy `src/content/docs` layout into the new content
// API. Without this file the build silently picks up zero pages —
// the home renders, but every linked page 404s.
import { defineCollection } from "astro:content";
import { docsLoader } from "@astrojs/starlight/loaders";
import { docsSchema } from "@astrojs/starlight/schema";

export const collections = {
  docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
};
