import { describe, expect, test } from "bun:test";
import { channel } from "./channel";

describe("channel", () => {
  test("builds outside Vercel, such as local and CI, are stable", () => {
    expect(channel({})).toEqual({ kind: "stable" });
    expect(channel({ VERCEL_ENV: "development", VERCEL_GIT_COMMIT_REF: "dev" })).toEqual({ kind: "stable" });
  });

  test("master's deployment is stable", () => {
    expect(channel({ VERCEL_ENV: "production", VERCEL_GIT_COMMIT_REF: "master" })).toEqual({ kind: "stable" });
  });

  test("the dev branch is dev, linking the stable docs", () => {
    expect(channel({ VERCEL_ENV: "preview", VERCEL_GIT_COMMIT_REF: "dev", VERCEL_PROJECT_PRODUCTION_URL: "docs.example.com" }))
      .toEqual({ kind: "dev", stableURL: "https://docs.example.com/docs" });
  });

  test("dev is dev even when it is the project's production branch", () => {
    expect(channel({ VERCEL_ENV: "production", VERCEL_GIT_COMMIT_REF: "dev" }).kind).toBe("dev");
  });

  test("another branch is a preview of it", () => {
    expect(channel({ VERCEL_ENV: "preview", VERCEL_GIT_COMMIT_REF: "feature/x" }))
      .toEqual({ kind: "preview", branch: "feature/x", stableURL: undefined });
  });

  test("a production deployment with no branch, such as from the CLI, is stable", () => {
    expect(channel({ VERCEL_ENV: "production" })).toEqual({ kind: "stable" });
  });

  test("a production deployment of another branch is labelled with it", () => {
    expect(channel({ VERCEL_ENV: "production", VERCEL_GIT_COMMIT_REF: "feature/x" }).kind).toBe("preview");
  });

  test("a preview deployment with no branch, such as from the CLI, is a preview", () => {
    expect(channel({ VERCEL_ENV: "preview" })).toEqual({ kind: "preview", branch: undefined, stableURL: undefined });
  });
});
