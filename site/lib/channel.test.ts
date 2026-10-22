import { describe, expect, test } from "bun:test";
import { channel } from "./channel";

describe("channel", () => {
  test("builds outside Vercel, such as local and CI, are stable", () => {
    expect(channel({})).toEqual({ kind: "stable" });
  });

  test("the production deployment is stable", () => {
    expect(channel({ VERCEL_ENV: "production", VERCEL_GIT_COMMIT_REF: "master" })).toEqual({ kind: "stable" });
  });

  test("the dev branch is dev, linking the stable docs", () => {
    expect(channel({ VERCEL_ENV: "preview", VERCEL_GIT_COMMIT_REF: "dev", VERCEL_PROJECT_PRODUCTION_URL: "docs.example.com" }))
      .toEqual({ kind: "dev", stableURL: "https://docs.example.com/docs" });
  });

  test("another branch is a preview of it", () => {
    expect(channel({ VERCEL_ENV: "preview", VERCEL_GIT_COMMIT_REF: "feature/x" }))
      .toEqual({ kind: "preview", branch: "feature/x", stableURL: undefined });
  });
});
