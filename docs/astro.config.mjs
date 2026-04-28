import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";

// Subpath deploy: gh-pages serves the chart at klinux.github.io/gocdnext/
// (helm repo metadata + tarballs at the root) and the docs at
// klinux.github.io/gocdnext/docs/. Astro needs `base` + `site` to mint
// asset URLs that resolve under the subpath.
export default defineConfig({
  site: "https://klinux.github.io",
  base: "/gocdnext/docs",
  integrations: [
    starlight({
      title: "gocdnext",
      description:
        "Modern CI/CD orchestrator with VSM, fanout and webhook-first ingest.",
      // Re-point Starlight's accent palette at the brand teal scale
      // used by the portal — see src/styles/brand.css for the
      // OKLCH stops that mirror web/app/globals.css --brand-*.
      customCss: ["./src/styles/brand.css"],
      // The SVG bakes the wordmark in (`gocd` in currentColor +
      // `next` in brand teal — mirroring web/components/brand/logo.tsx)
      // so replacesTitle keeps the docs header visually identical
      // to the portal's sidebar brand.
      logo: { src: "./src/assets/logo.svg", replacesTitle: true },
      social: {
        github: "https://github.com/klinux/gocdnext",
      },
      editLink: {
        baseUrl: "https://github.com/klinux/gocdnext/edit/main/docs/",
      },
      // Pagefind-based search ships with Starlight, no external service.
      // Indexes the built site at `astro build` time.
      sidebar: [
        {
          label: "Start here",
          items: [
            { label: "What is gocdnext?", link: "/" },
            { label: "Quickstart", link: "/pipelines/quickstart/" },
          ],
        },
        {
          label: "Install & operate",
          items: [
            { label: "Helm install", link: "/install/helm/" },
            { label: "Local dev", link: "/install/local-dev/" },
            { label: "Authentication", link: "/install/auth/" },
            { label: "Upgrade runbook", link: "/install/upgrade/" },
          ],
        },
        {
          label: "Author pipelines",
          items: [
            { label: "Quickstart", link: "/pipelines/quickstart/" },
            { label: "YAML reference", link: "/pipelines/yaml-reference/" },
            {
              label: "Recipes",
              items: [
                {
                  label: "Go monorepo",
                  link: "/pipelines/recipes/go-monorepo/",
                },
                {
                  label: "Maven (Java/Kotlin)",
                  link: "/pipelines/recipes/maven/",
                },
                {
                  label: "Gradle (Android, JVM)",
                  link: "/pipelines/recipes/gradle/",
                },
                {
                  label: "Node frontend",
                  link: "/pipelines/recipes/node/",
                },
                {
                  label: "Python (pip / Poetry / uv)",
                  link: "/pipelines/recipes/python/",
                },
                {
                  label: "Deploy via SSH",
                  link: "/pipelines/recipes/ssh-deploy/",
                },
              ],
            },
          ],
        },
        {
          label: "Reference",
          items: [{ label: "Plugin catalog", link: "/reference/plugins/" }],
        },
      ],
    }),
  ],
});
