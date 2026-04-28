// Build-time generator for the plugin catalog page. Reads every
// `plugin.yaml` under ../plugins/<name>/ and writes a single Markdown
// file at src/content/docs/reference/plugins.md. The output mirrors
// the same manifest shape the server validates `with:` against, so
// the docs and the runtime stay locked.
//
// Run via `pnpm gen` or implicitly by `pnpm build`.

import { readdir, readFile, writeFile, mkdir } from "node:fs/promises";
import { existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import YAML from "yaml";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(__dirname, "..", "..");
const PLUGINS_DIR = path.join(REPO_ROOT, "plugins");
const OUT_PATH = path.resolve(
  __dirname,
  "..",
  "src",
  "content",
  "docs",
  "reference",
  "plugins.md",
);

async function loadManifests() {
  const entries = await readdir(PLUGINS_DIR, { withFileTypes: true });
  const out = [];
  for (const e of entries) {
    if (!e.isDirectory()) continue;
    const manifestPath = path.join(PLUGINS_DIR, e.name, "plugin.yaml");
    if (!existsSync(manifestPath)) continue;
    const raw = await readFile(manifestPath, "utf8");
    let parsed;
    try {
      parsed = YAML.parse(raw);
    } catch (err) {
      console.warn(`skip ${e.name}: parse error: ${err.message}`);
      continue;
    }
    if (!parsed?.name) {
      console.warn(`skip ${e.name}: manifest missing 'name'`);
      continue;
    }
    out.push({ dir: e.name, ...parsed });
  }
  // Group by category, sort by name within group. Categories without
  // an entry are dropped from the output, so adding a new one is just
  // setting `category:` in the manifest.
  out.sort((a, b) => a.name.localeCompare(b.name));
  return out;
}

function escapeMd(s) {
  if (s == null) return "";
  return String(s)
    .replace(/\|/g, "\\|")
    .replace(/\r?\n/g, " ")
    .trim();
}

function renderInputsTable(inputs) {
  if (!inputs || Object.keys(inputs).length === 0) {
    return "_No inputs._\n";
  }
  let s = "| Input | Required | Default | Description |\n";
  s += "|---|---|---|---|\n";
  for (const [key, spec] of Object.entries(inputs)) {
    const req = spec?.required ? "yes" : "no";
    const def =
      spec?.default !== undefined && spec?.default !== ""
        ? `\`${spec.default}\``
        : "—";
    s += `| \`${key}\` | ${req} | ${def} | ${escapeMd(spec?.description)} |\n`;
  }
  return s + "\n";
}

function renderExamples(examples) {
  if (!examples?.length) return "";
  let s = "**Examples**\n\n";
  for (const ex of examples) {
    s += `### ${escapeMd(ex.name) || "example"}\n\n`;
    if (ex.description) s += `${escapeMd(ex.description)}\n\n`;
    if (ex.yaml) {
      s += "```yaml\n" + ex.yaml.trimEnd() + "\n```\n\n";
    }
  }
  return s;
}

function renderPlugin(p) {
  let s = `## ${p.name} {#${p.name}}\n\n`;
  if (p.category) {
    s += `_${p.category}_ — `;
  }
  s += `${escapeMd(p.description)}\n\n`;
  s += `Image: \`ghcr.io/klinux/gocdnext-plugin-${p.dir}:v1\`\n\n`;
  s += "**Inputs**\n\n";
  s += renderInputsTable(p.inputs);
  s += renderExamples(p.examples);
  return s;
}

function renderHeader(plugins) {
  const byCat = {};
  for (const p of plugins) {
    const c = p.category || "uncategorized";
    (byCat[c] ||= []).push(p);
  }
  const cats = Object.keys(byCat).sort();

  let s = "---\n";
  s += "title: Plugin catalog\n";
  s +=
    'description: Every plugin in the official gocdnext catalog, generated at build time from the `plugin.yaml` manifests the server validates against.\n';
  s += "---\n\n";
  s +=
    "These pages are generated from the manifests at " +
    "[`plugins/<name>/plugin.yaml`](https://github.com/klinux/gocdnext/tree/main/plugins) " +
    "at every doc build. The schemas you see here are the ones the " +
    "control plane uses to validate `with:` blocks at apply time, " +
    "so what's written down matches what runs.\n\n";
  s += "## At a glance\n\n";
  for (const cat of cats) {
    s += `**${cat}** — `;
    s +=
      byCat[cat]
        .map((p) => `[${p.name}](#${p.name})`)
        .join(", ") + "\n\n";
  }
  return s;
}

async function main() {
  const plugins = await loadManifests();
  const out =
    renderHeader(plugins) + plugins.map(renderPlugin).join("\n");
  await mkdir(path.dirname(OUT_PATH), { recursive: true });
  await writeFile(OUT_PATH, out, "utf8");
  console.log(
    `gen-plugin-catalog: wrote ${plugins.length} plugins to ${path.relative(
      REPO_ROOT,
      OUT_PATH,
    )}`,
  );
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
