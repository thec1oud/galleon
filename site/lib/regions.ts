// Code samples in the docs are copied from Go files that CI compiles and
// tests, so they can't go stale. A page names its source in the code fence:
//
//	```go file=examples/firstapp/greetings.go#module title="greetings.go"
//	...
//	```
//
// and scripts/sync-samples.ts rewrites the fence's body from that file. A
// region is the lines between
//
//	// docs:start NAME
//	// docs:end NAME
//
// Marker lines of any region are dropped from a sample, so regions can nest
// or overlap. Blank lines at either end and the common leading indentation
// are removed. Without #NAME, the sample is the whole file.

import type { Root } from "mdast";
import { frontmatterFromMarkdown } from "mdast-util-frontmatter";
import { fromMarkdown } from "mdast-util-from-markdown";
import { gfmFromMarkdown } from "mdast-util-gfm";
import { mdxFromMarkdown } from "mdast-util-mdx";
import { frontmatter } from "micromark-extension-frontmatter";
import { gfm } from "micromark-extension-gfm";
import { mdxjs } from "micromark-extension-mdxjs";
import { visit } from "unist-util-visit";

const MARKER = /^\s*\/\/ docs:(start|end) ([\w-]+)\s*$/;

/**
 * Returns region `name` of `source`, or all of it when `name` is empty.
 * Throws if the region is missing, unclosed or repeated.
 */
export function extractRegion(source: string, name: string, label = "source"): string {
  const lines = source.replace(/\r\n/g, "\n").replace(/\n$/, "").split("\n");
  let out: string[] = [];
  if (!name) {
    out = lines.filter((l) => !MARKER.test(l));
  } else {
    let state: "before" | "in" | "after" = "before";
    for (const line of lines) {
      const m = MARKER.exec(line);
      if (m && m[2] === name) {
        if (m[1] === "start") {
          if (state !== "before") throw new Error(`${label}: region "${name}" starts twice`);
          state = "in";
        } else {
          if (state !== "in") throw new Error(`${label}: region "${name}" ends before it starts`);
          state = "after";
        }
        continue;
      }
      if (state === "in" && !m) out.push(line);
    }
    if (state === "before") throw new Error(`${label}: no region "${name}"`);
    if (state === "in") throw new Error(`${label}: region "${name}" is never closed`);
  }
  while (out.length > 0 && out[0].trim() === "") out.shift();
  while (out.length > 0 && out[out.length - 1].trim() === "") out.pop();
  return dedent(out).join("\n");
}

function dedent(lines: string[]): string[] {
  const indents = lines.filter((l) => l.trim() !== "").map((l) => /^[\t ]*/.exec(l)![0]);
  if (indents.length === 0) return lines;
  let common = indents[0];
  for (const ind of indents) {
    let i = 0;
    while (i < common.length && i < ind.length && common[i] === ind[i]) i++;
    common = common.slice(0, i);
  }
  return lines.map((l) => (l.startsWith(common) ? l.slice(common.length) : l.trimStart()));
}

/**
 * Rewrites the body of every fenced code block in `page` (MDX) whose info
 * string names a `file=PATH[#REGION]`, using `read(PATH)` for the file's
 * contents. The page is parsed as MDX, or as plain Markdown with format
 * "md" (as GitHub reads a README, with HTML comments and tags), so only real
 * code blocks count: an example of the syntax inside a longer fence is left
 * alone. Line endings become LF.
 */
export function syncSamples(page: string, read: (path: string) => string, label = "page", format: "mdx" | "md" = "mdx"): string {
  const source = page.replace(/\r\n/g, "\n");
  const mdx = format === "mdx";
  let tree: Root;
  try {
    tree = fromMarkdown(source, {
      extensions: [...(mdx ? [mdxjs()] : []), gfm(), frontmatter(["yaml"])],
      mdastExtensions: [...(mdx ? [mdxFromMarkdown()] : []), gfmFromMarkdown(), frontmatterFromMarkdown(["yaml"])],
    });
  } catch (e) {
    throw new Error(`${label}: ${(e as Error).message}`);
  }
  const lines = source.split("\n");
  const edits: { from: number; to: number; body: string[] }[] = [];
  visit(tree, "code", (node) => {
    const file = metaFile(node.meta ?? "");
    if (!file || !node.position) return;
    const first = node.position.start.line - 1;
    // A block that ends where its container does ends at column 1 of the
    // next line, which isn't part of it.
    const end = node.position.end;
    const last = end.column === 1 ? end.line - 2 : end.line - 1;
    // What precedes the fence on its line, with any list marker blanked out,
    // continues each line of the sample: indentation, or "> " in a quote.
    const opener = lines[first].slice(0, node.position.start.column - 1);
    const prefix = opener.replace(/[^>\s]/g, " ");
    const fence = /^(`{3,}|~{3,})/.exec(lines[first].slice(opener.length))![1];
    // CommonMark lets a list item or quote end a block, but a sample must
    // close its own fence, so a forgotten fence can't swallow what follows.
    if (last <= first || !isClosing(withoutContainer(lines[last], prefix), fence)) {
      throw new Error(`${label}:${first + 1}: code fence is never closed`);
    }
    const [path, name, ...extra] = file.split("#");
    if (name === "" || extra.length > 0) throw new Error(`${label}:${first + 1}: file=${file} should be PATH or PATH#REGION`);
    const sample = extractRegion(read(path), name ?? "", path).split("\n");
    if (sample.some((l) => isClosing(l, fence))) {
      throw new Error(`${label}:${first + 1}: ${file} has a line that would close the fence; use a longer fence`);
    }
    edits.push({ from: first + 1, to: last, body: sample.map((l) => (l === "" ? prefix.trimEnd() : prefix + l)) });
  });
  for (const e of edits.reverse()) lines.splice(e.from, e.to - e.from, ...e.body);
  return lines.join("\n");
}

// metaFile returns the value of the file= attribute in a code block's meta
// string, skipping quoted values of other attributes.
function metaFile(meta: string): string | undefined {
  for (const m of meta.matchAll(/([\w-]+)=("[^"]*"|'[^']*'|\S+)|"[^"]*"|'[^']*'|\S+/g)) {
    if (m[1] === "file") return m[2].replace(/^(["'])(.*)\1$/, "$2");
  }
  return undefined;
}

// withoutContainer removes from line what its container contributes, as
// described by prefix: a ">" for each quote level and the indentation of
// list items. What remains keeps the fence's own indentation.
function withoutContainer(line: string, prefix: string): string {
  let rest = line;
  for (const ch of prefix) {
    if (ch === ">") rest = rest.replace(/^[ \t]*>/, "");
    else if (rest[0] === " " || rest[0] === "\t") rest = rest.slice(1);
  }
  return rest;
}

// isClosing reports whether line would close a code block opened by fence.
function isClosing(line: string, fence: string): boolean {
  const m = /^ {0,3}(`{3,}|~{3,})[ \t]*$/.exec(line);
  return m !== null && m[1][0] === fence[0] && m[1].length >= fence.length;
}
