// Rewrites every code sample in the docs, and in the README, from the Go
// file it names (see lib/regions.ts). With --check, changes nothing and
// fails if any page is out of date, which is what CI runs.
//
// Pages carry their samples in their own source, rather than having them
// filled in at build time, so that the Markdown versions of pages served to
// agents (/docs/<page>.md) show the code too.

import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { syncSamples } from "../lib/regions";

const SITE = path.resolve(import.meta.dir, "..");
const PAGES = path.join(SITE, "app/docs");
// Generated from docs/adr by sync-adrs.ts, which has no samples.
const DECISIONS = path.join(PAGES, "decisions");
// The repository's README shows samples too, with paths relative to the site
// like a page's.
const README = path.join(SITE, "..", "README.md");

function* pages(dir: string): Generator<string> {
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) {
      if (p !== DECISIONS) yield* pages(p);
    }
    else if (/\.mdx?$/.test(e.name)) yield p;
  }
}

function read(rel: string): string {
  const abs = path.resolve(SITE, rel);
  if (!abs.startsWith(SITE + path.sep)) throw new Error(`${rel} is outside the site`);
  try {
    return readFileSync(abs, "utf8");
  } catch {
    throw new Error(`can't read ${rel}`);
  }
}

const check = process.argv.includes("--check");
const stale: string[] = [];
for (const page of [...pages(PAGES), README]) {
  const rel = path.relative(SITE, page);
  const before = readFileSync(page, "utf8");
  const after = syncSamples(before, read, rel, page === README ? "md" : "mdx");
  // A CRLF checkout (Git's core.autocrlf on Windows) isn't out of date.
  if (after === before.replace(/\r\n/g, "\n")) continue;
  stale.push(rel);
  if (!check) writeFileSync(page, after);
}
if (check && stale.length > 0) {
  console.error(`sync-samples: out of date (run \`bun run samples\`):\n  ${stale.join("\n  ")}`);
  process.exit(1);
}
console.log(`sync-samples: ${check ? "all up to date" : `${stale.length} page(s) updated`}`);
