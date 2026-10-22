import { describe, expect, test } from "bun:test";
import { extractRegion, syncSamples } from "./regions";

const src = [
  "package main",
  "",
  "func main() {",
  "\t// docs:start body",
  "\tx := 1",
  "\t// docs:start inner",
  "\tif x > 0 {",
  "\t\tprintln(x)",
  "\t}",
  "\t// docs:end inner",
  "\t// docs:end body",
  "}",
  "",
].join("\n");

describe("extractRegion", () => {
  test("returns a region, dedented, without nested markers", () => {
    expect(extractRegion(src, "body")).toBe("x := 1\nif x > 0 {\n\tprintln(x)\n}");
  });
  test("returns a nested region", () => {
    expect(extractRegion(src, "inner")).toBe("if x > 0 {\n\tprintln(x)\n}");
  });
  test("returns the whole file without markers when no region is named", () => {
    expect(extractRegion(src, "")).not.toContain("docs:");
    expect(extractRegion(src, "")).toStartWith("package main");
  });
  test("handles CRLF", () => {
    expect(extractRegion(src.replace(/\n/g, "\r\n"), "inner")).toBe("if x > 0 {\n\tprintln(x)\n}");
  });
  test("refuses a missing region", () => {
    expect(() => extractRegion(src, "nope")).toThrow('no region "nope"');
  });
  test("refuses an unclosed region", () => {
    expect(() => extractRegion("// docs:start a\nx", "a")).toThrow("never closed");
  });
  test("refuses a repeated region", () => {
    expect(() => extractRegion("// docs:start a\n// docs:end a\n// docs:start a\n// docs:end a", "a")).toThrow("starts twice");
  });
  test("trims blank lines at either end", () => {
    expect(extractRegion("// docs:start a\n\nx\n\n// docs:end a", "a")).toBe("x");
  });
  test("refuses an end before a start", () => {
    expect(() => extractRegion("// docs:end a\n// docs:start a", "a")).toThrow("ends before");
  });
});

describe("syncSamples", () => {
  const read = (p: string) => {
    if (p === "main.go") return src;
    throw new Error(`can't read ${p}`);
  };

  test("fills a fence from a file and region, keeping its info string", () => {
    const page = 'Intro\n\n```go file=main.go#inner title="main.go"\nstale\n```\n\nAfter\n';
    expect(syncSamples(page, read)).toBe(
      'Intro\n\n```go file=main.go#inner title="main.go"\nif x > 0 {\n\tprintln(x)\n}\n```\n\nAfter\n',
    );
  });
  test("is idempotent", () => {
    const once = syncSamples("```go file=main.go#body\n```\n", read);
    expect(syncSamples(once, read)).toBe(once);
  });
  test("indents a sample inside an indented fence", () => {
    expect(syncSamples("1. Step\n\n   ```go file=main.go#inner\n   ```\n", read)).toBe(
      "1. Step\n\n   ```go file=main.go#inner\n   if x > 0 {\n   \tprintln(x)\n   }\n   ```\n",
    );
  });
  test("leaves fences without file= alone", () => {
    const page = "```go\nfmt.Println()\n```\n";
    expect(syncSamples(page, read)).toBe(page);
  });
  test("leaves a fence nested in a longer fence alone", () => {
    const page = "````md\n```go file=missing.go#x\n```\n````\n";
    expect(syncSamples(page, read)).toBe(page);
  });
  test("fails on a missing file or region", () => {
    expect(() => syncSamples("```go file=missing.go\n```\n", read)).toThrow("can't read");
    expect(() => syncSamples("```go file=main.go#nope\n```\n", read)).toThrow("no region");
  });
  test("reads plain Markdown, as GitHub does a README, with format md", () => {
    const page = "<!-- a note -->\n\n<details>\n\n```go file=main.go#inner\n```\n\n</details>\n";
    expect(() => syncSamples(page, read)).toThrow();
    expect(syncSamples(page, read, "README.md", "md")).toBe(
      "<!-- a note -->\n\n<details>\n\n```go file=main.go#inner\nif x > 0 {\n\tprintln(x)\n}\n```\n\n</details>\n",
    );
  });
  test("handles a CRLF page", () => {
    expect(syncSamples("```go file=main.go#inner\r\nstale\r\n```\r\n", read)).toBe(
      "```go file=main.go#inner\nif x > 0 {\n\tprintln(x)\n}\n```\n",
    );
  });
  test("fails on a sample with a line that would close the fence", () => {
    const md = (p: string) => (p === "a.md" ? "x\n```\ny" : read(p));
    expect(() => syncSamples("```md file=a.md\n```\n", md)).toThrow("would close the fence");
    expect(syncSamples("````md file=a.md\n````\n", md)).toBe("````md file=a.md\nx\n```\ny\n````\n");
  });
  test("fills a fence inside a block quote or a component", () => {
    expect(syncSamples("> ```go file=main.go#inner\n> ```\n", read)).toBe(
      "> ```go file=main.go#inner\n> if x > 0 {\n> \tprintln(x)\n> }\n> ```\n",
    );
    expect(syncSamples("<Callout>\n\n```go file=main.go#inner\n```\n\n</Callout>\n", read)).toContain("\tprintln(x)");
  });
  test("fills a fence in a list item without repeating the marker", () => {
    expect(syncSamples("- ```go file=main.go#inner\n  ```\n", read)).toBe(
      "- ```go file=main.go#inner\n  if x > 0 {\n  \tprintln(x)\n  }\n  ```\n",
    );
    expect(syncSamples("> 1. ```go file=main.go#inner\n>    ```\n", read)).toBe(
      "> 1. ```go file=main.go#inner\n>    if x > 0 {\n>    \tprintln(x)\n>    }\n>    ```\n",
    );
  });
  test("accepts a closing fence without a space after the quote marker", () => {
    expect(syncSamples("> ```go file=main.go#inner\n>```\n", read)).toContain(">```");
  });
  test("refuses a fence closed only by the end of its quote or list item", () => {
    expect(() => syncSamples("> ```go file=main.go#inner\n> stale\n\nAfter\n", read)).toThrow("never closed");
    const steps = "1. Create the file:\n   ```go file=main.go#inner\n   old\n2. Run it:\n   ```sh\n   go run .\n   ```\n";
    expect(() => syncSamples(steps, read)).toThrow("never closed");
    expect(() => syncSamples("- ```go file=main.go#inner\n  old\n- next\n", read)).toThrow("never closed");
  });
  test("doesn't take an indented code line for the closing fence", () => {
    expect(() => syncSamples("> ```go file=main.go#inner\n>     ```\n\nAfter\n", read)).toThrow("never closed");
  });
  test("refuses a malformed file=", () => {
    expect(() => syncSamples("```go file=main.go#\n```\n", read)).toThrow("PATH#REGION");
    expect(() => syncSamples("```go file=main.go#a#b\n```\n", read)).toThrow("PATH#REGION");
  });
  test("reads file= only as its own attribute", () => {
    expect(syncSamples('```go title="see file=x.go" file=main.go#inner\n```\n', read)).toContain("\tprintln(x)");
    const page = '```go title="see file=x.go"\nhand\n```\n';
    expect(syncSamples(page, read)).toBe(page);
  });
  test("isn't fooled by inline code that looks like a fence", () => {
    expect(syncSamples("``` `x` ```\n\n```go file=main.go#inner\n```\n", read)).toContain("\tprintln(x)");
  });
  test("skips the frontmatter", () => {
    expect(syncSamples("---\ntitle: x\n---\n\n```go file=main.go#inner\n```\n", read)).toContain("\tprintln(x)");
  });
  test("fails on an unclosed fence", () => {
    expect(() => syncSamples("```go file=main.go\n", read)).toThrow("never closed");
  });
});
