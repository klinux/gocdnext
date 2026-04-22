import type { NextConfig } from "next";

const config: NextConfig = {
  reactStrictMode: true,
  // typedRoutes moved out of `experimental` in Next 15 — the legacy
  // location still works but logs a deprecation warning on every
  // build that pollutes the dev-server output.
  typedRoutes: true,
};

export default config;
