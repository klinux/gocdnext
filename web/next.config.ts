import type { NextConfig } from "next";

const config: NextConfig = {
  reactStrictMode: true,
  // Produce a self-contained prod build under `.next/standalone/`
  // so the `bundle` CI job has something deterministic to ship as
  // an artifact — without this flag `next build` only fills
  // `.next/` with intermediate server files (not meant for deploy)
  // and the standalone dir the pipeline uploads simply doesn't
  // exist, failing the upload.
  output: "standalone",
  // typedRoutes moved out of `experimental` in Next 15 — the legacy
  // location still works but logs a deprecation warning on every
  // build that pollutes the dev-server output.
  typedRoutes: true,
};

export default config;
