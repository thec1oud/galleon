// Copies the architecture decision records from docs/adr into the site's
// Decisions section (app/docs/decisions), which is generated and not
// committed. docs/adr stays the source of truth: records are written and
// reviewed there, as plain Markdown that reads well on GitHub too.
//

import { mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import type { Paragraph, Text } from "mdast";
import { fromMarkdown } from "mdast-util-from-markdown";
import { gfmFromMarkdown, gfmToMarkdown } from "mdast-util-gfm";
import { mdxToMarkdown } from "mdast-util-mdx";
import { toMarkdown, type Unsafe } from "mdast-util-to-markdown";
import { toString } from "mdast-util-to-string";
import { gfm } from "micromark-extension-gfm";
import { SKIP, visit } from "unist-util-visit";

const SITE = path.resolve(import.meta.dir, "..");
const REPO = path.resolve(SITE, "..");
const ADR_DIR = path.join(REPO, "docs/adr");
const OUT = path.join(SITE, "app/docs/decisions");
const GITHUB = "https://github.com/thec1oud/galleon/blob/dev/";
const RECORD = /^(\d{4})-[a-z0-9-]+\.md$/;

export type AdrRecord = {
  file: string;
  number: string;
  title: string;
  status: string;
  date: string;
  body: string;
};

export function parseRecord(file: string, text: string): AdrRecord {
  const m = RECORD.exec(file);
  if (!m) throw new Error(`${file}: not a record`);
  const lines = text.replace(/\r\n/g, "\n").split("\n");
  const title = /^# \d{4}\. (.+)$/.exec(lines[0] ?? "")?.[1];
  const status = /^Status: (.+)$/.exec(lines[2] ?? "")?.[1];
  const date = /^Date: (\d{4}-\d{2}-\d{2})$/.exec(lines[3] ?? "")?.[1];
  if (!title || !status || !date) {
    throw new Error(`${file}: expected "# NNNN. Title", "Status:" and "Date:" on lines 1, 3 and 4`);
  }
  return { file, number: m[1], title, status, date, body: lines.slice(4).join("\n").trim() };
}

const MDX_UNSAFE: Unsafe[] = [
  // A line starting with import or export is an ES module statement in MDX.
  { character: "i", atBreak: true, after: "mport\\b" },
  { character: "e", atBreak: true, after: "xport\\b" },
];

const PHRASING = new Set(["paragraph", "heading", "tableCell", "emphasis", "strong", "delete", "link", "linkReference"]);

// toMdx converts a Markdown file in docs/adr to MDX that compiles and
// renders the same. It parses the Markdown (CommonMark and GFM) and
// serializes it as MDX, which escapes "{" and "<" wherever MDX would read
// them as an expression or a tag, and leaves code alone. HTML comments are
// dropped and other raw HTML is shown as text, since MDX supports neither.
// Relative links and images become site or GitHub URLs (see linkTarget).
export function toMdx(markdown: string): string {
  const tree = fromMarkdown(markdown, { extensions: [gfm()], mdastExtensions: [gfmFromMarkdown()] });
  visit(tree, (node, index, parent) => {
    if (node.type === "link" || node.type === "definition") node.url = linkTarget(node.url, "blob");
    if (node.type === "image") node.url = linkTarget(node.url, "raw");
    if (node.type === "html" && parent && index !== undefined) {
      const value = node.value.replace(/<!--[\s\S]*?-->/g, "");
      if (value.trim() === "") {
        parent.children.splice(index, 1);
        return [SKIP, index];
      }
      const text: Text = { type: "text", value };
      parent.children[index] = PHRASING.has(parent.type) ? text : ({ type: "paragraph", children: [text] } as Paragraph);
    }
  });
  return toMarkdown(tree, {
    bullet: "-",
    extensions: [gfmToMarkdown(), mdxToMarkdown(), { unsafe: MDX_UNSAFE }],
  }).trimEnd();
}

// linkTarget rewrites one link target, relative to docs/adr: a record
// becomes its page, the README the Decisions page, and any other file in the
// repository a GitHub URL on the dev branch ("raw" for images).
export function linkTarget(target: string, kind: "blob" | "raw"): string {
  if (/^[a-z][a-z0-9+.-]*:/i.test(target) || target.startsWith("#") || target.startsWith("/") || target === "") return target;
  const cut = target.search(/[?#]/);
  const file = (cut < 0 ? target : target.slice(0, cut)).replace(/^(\.\/)+/, "");
  const hashAt = target.indexOf("#");
  const hash = hashAt < 0 ? "" : target.slice(hashAt);
  if (kind === "blob" && RECORD.test(file)) return `/docs/decisions/${file.replace(/\.md$/, "")}${hash}`;
  if (kind === "blob" && file === "README.md") return `/docs/decisions${hash}`;
  const repoPath = path.posix.normalize(path.posix.join("docs/adr", file));
  if (repoPath.startsWith("..")) return target;
  return `${GITHUB.replace("/blob/", `/${kind}/`)}${repoPath}${hash}`;
}

// plainText returns the text of a line of Markdown, without its markup.
function plainText(markdown: string): string {
  return toString(fromMarkdown(markdown, { extensions: [gfm()], mdastExtensions: [gfmFromMarkdown()] }));
}

export function renderRecord(r: AdrRecord): string {
  return [
    "---",
    // The sidebar and <title> show the title as plain text.
    `title: ${JSON.stringify(`${r.number}. ${plainText(r.title)}`)}`,
    `description: ${JSON.stringify(`${r.status.replace(/ by \[.*$/, "")}, ${r.date}`)}`,
    `order: ${Number(r.number)}`,
    "---",
    "",
    toMdx(`# ${r.number}. ${r.title}`),
    "",
    `**Status:** ${toMdx(r.status)} · **Date:** ${r.date} · [Source on GitHub](${GITHUB}docs/adr/${r.file})`,
    "",
    toMdx(r.body),
    "",
  ].join("\n");
}

export function renderIndex(readme: string): string {
  const body = readme.replace(/\r\n/g, "\n").replace(/^# .*\n/, "").trim();
  return [
    "---",
    'title: "Decisions"',
    'description: "Why Galleon is built the way it is: every architecture decision, with the options we rejected."',
    "order: 5",
    "---",
    "",
    "# Decisions",
    "",
    toMdx(body),
    "",
  ].join("\n");
}

export function sync(): number {
  rmSync(OUT, { recursive: true, force: true });
  mkdirSync(OUT, { recursive: true });
  writeFileSync(path.join(OUT, "page.mdx"), renderIndex(readFileSync(path.join(ADR_DIR, "README.md"), "utf8")));
  const files = readdirSync(ADR_DIR).filter((f) => RECORD.test(f)).sort();
  for (const file of files) {
    const r = parseRecord(file, readFileSync(path.join(ADR_DIR, file), "utf8"));
    const dir = path.join(OUT, file.replace(/\.md$/, ""));
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, "page.mdx"), renderRecord(r));
  }
  return files.length;
}

if (import.meta.main) {
  console.log(`sync-adrs: ${sync()} record(s) into ${path.relative(SITE, OUT)}`);
}
