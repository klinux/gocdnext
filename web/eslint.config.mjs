import next from "eslint-config-next";

// Flat config (ESLint 9). `eslint-config-next` already bundles the
// Next core-web-vitals + TypeScript rules and ignores node_modules/.git
// by default; we add the build/output dirs. `next lint` was removed in
// Next 16, so the `lint` script calls `eslint` directly against this.
/** @type {import('eslint').Linter.Config[]} */
const config = [
  ...next,
  {
    ignores: [".next/**", "out/**", "next-env.d.ts"],
  },
];

export default config;
