import { describe, expect, test } from "bun:test";
import { parseRecord, renderRecord, toMdx } from "./sync-adrs";

const record = [
  "# 0002. Rank things",
  "",
  "Status: Superseded by [0009](0009-new-ranking.md)",
  "Date: 2026-10-20",
  "",
  "## Context",
  "",
  "See [0001](0001-record.md#context), [the template](template.md), the",
  "[check](../../.github/scripts/adr-check.sh), [prior art](https://example.com/x.md)",
  "and [above](#context). Code stays: `[x](0001-record.md)`.",
  "",
].join("\n");

describe("parseRecord", () => {
  test("reads the header", () => {
    const r = parseRecord("0002-rank-things.md", record);
    expect(r).toMatchObject({ number: "0002", title: "Rank things", date: "2026-10-20" });
    expect(r.body).toStartWith("## Context");
  });
  test("refuses a malformed record", () => {
    expect(() => parseRecord("0002-x.md", "# Nope\n")).toThrow("expected");
  });
});

describe("toMdx links", () => {
  const out = toMdx(record);
  test("turns record links into site links, keeping the anchor", () => {
    expect(out).toContain("](/docs/decisions/0001-record#context)");
    expect(out).toContain("](/docs/decisions/0009-new-ranking)");
  });
  test("turns other repository files into GitHub links", () => {
    expect(out).toContain("](https://github.com/thec1oud/galleon/blob/dev/docs/adr/template.md)");
    expect(out).toContain("](https://github.com/thec1oud/galleon/blob/dev/.github/scripts/adr-check.sh)");
  });
  test("leaves absolute links, anchors and code alone", () => {
    expect(out).toContain("](https://example.com/x.md)");
    expect(out).toContain("](#context)");
    expect(out).toContain("`[x](0001-record.md)`");
  });
  test("handles ./ prefixes, titles, angle brackets and reference definitions", () => {
    expect(toMdx("[a](./0001-record.md)")).toBe("[a](/docs/decisions/0001-record)");
    expect(toMdx('[a](0001-record.md "First")')).toBe('[a](/docs/decisions/0001-record "First")');
    expect(toMdx("[a](<0001-record.md>)")).toBe("[a](/docs/decisions/0001-record)");
    expect(toMdx("[a][r]\n\n[r]: 0001-record.md#x")).toBe("[a][r]\n\n[r]: /docs/decisions/0001-record#x");
  });
  test("keeps every fragment, and drops a query from a record link", () => {
    expect(toMdx("[a](0001-record.md#a#b)")).toBe("[a](/docs/decisions/0001-record#a#b)");
    expect(toMdx("[a](0001-record.md?plain=1#c)")).toBe("[a](/docs/decisions/0001-record#c)");
  });
  test("points images at raw files", () => {
    expect(toMdx("![d](flow.png)")).toBe("![d](https://github.com/thec1oud/galleon/raw/dev/docs/adr/flow.png)");
  });
  test("leaves footnotes alone", () => {
    expect(toMdx("A[^1].\n\n[^1]: See the template.")).toBe("A[^1].\n\n[^1]: See the template.");
  });
});

describe("toMdx escaping", () => {
  test("escapes braces and angle brackets outside code only", () => {
    const out = toMdx("a {b} <c> `{d} <e>`\n\n```go\nf := map[string]int{}\n```");
    expect(out).toBe("a \\{b} \\<c> `{d} <e>`\n\n```go\nf := map[string]int{}\n```");
  });
  test("keeps the author's own escapes", () => {
    expect(toMdx("\\{a\\} \\<b \\`{c}\\`")).toBe("\\{a} \\<b \\`\\{c}\\`");
  });
  test("leaves tilde fences, indented fences and double-backtick spans alone", () => {
    expect(toMdx("~~~\n{a}\n~~~")).toContain("{a}");
    expect(toMdx("- x\n\n  ```\n  <b>\n  ```")).toContain("  <b>");
    expect(toMdx("``x ` {c}``")).toBe("``x ` {c}``");
  });
  test("doesn't pair a stray backtick with one paragraphs later", () => {
    expect(toMdx("a ` {b}\n\nc ` d")).toBe("a \\` \\{b}\n\nc \\` d");
  });
  test("converts autolinks, drops comments and escapes import and export lines", () => {
    expect(toMdx("See <https://x.dev/a>.\n\n<!-- use `x` here -->\n\nimport x\nexport y\nreimport")).toBe(
      "See [https://x.dev/a](https://x.dev/a).\n\n&#x69;mport x\n&#x65;xport y\nreimport",
    );
  });
  test("drops a comment that shares its line with text", () => {
    const out = toMdx("<!-- c --> text");
    expect(out).not.toContain("c -->");
    expect(out).toEndWith("text");
  });
  test("shows raw HTML as text", () => {
    expect(toMdx("<div>\n{a}\n</div>")).toBe("\\<div>\n\\{a}\n\\</div>");
  });
});

describe("renderRecord", () => {
  const page = renderRecord(parseRecord("0002-rank-things.md", record));
  test("writes frontmatter with the number, status and order", () => {
    expect(page).toContain('title: "0002. Rank things"');
    expect(page).toContain('description: "Superseded, 2026-10-20"');
    expect(page).toContain("order: 2");
  });
  test("puts a plain-text title in the frontmatter", () => {
    const r = parseRecord("0003-x.md", record.replace("# 0002. Rank things", "# 0003. Use *stars* and `x`"));
    expect(renderRecord(r)).toContain('title: "0003. Use stars and x"');
  });
  test("links the superseding record in the status line", () => {
    expect(page).toContain("**Status:** Superseded by [0009](/docs/decisions/0009-new-ranking)");
  });
});
