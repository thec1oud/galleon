// Which copy of the docs a build is, from the system environment variables
// Vercel sets at build time. The stable docs are master's deployment, and
// any build outside Vercel, such as `bun run dev` and CI; the dev branch's
// deployment is "dev"; any other deployment is a "preview" of its branch.
// It goes by the branch rather than VERCEL_ENV alone, so a project whose
// Production Branch was left at the repository's default (dev) still labels
// dev as dev.

export type Channel =
  | { kind: "stable" }
  | { kind: "dev"; stableURL?: string }
  | { kind: "preview"; branch?: string; stableURL?: string };

export function channel(env: Record<string, string | undefined> = process.env): Channel {
  if (env.VERCEL_ENV !== "preview" && env.VERCEL_ENV !== "production") return { kind: "stable" };
  const branch = env.VERCEL_GIT_COMMIT_REF || undefined;
  if (branch === "master") return { kind: "stable" };
  // The production domain, without a scheme, such as docs.example.com.
  const host = env.VERCEL_PROJECT_PRODUCTION_URL;
  const stableURL = host ? `https://${host}/docs` : undefined;
  if (branch === "dev") return { kind: "dev", stableURL };
  return { kind: "preview", branch, stableURL };
}
