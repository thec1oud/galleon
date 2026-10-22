// Which copy of the docs a build is, from the system environment variables
// Vercel sets at build time. Production (master) and builds elsewhere, such
// as `bun run dev` and CI, are "stable"; the dev branch's deployment is
// "dev"; any other branch's is a "preview".

export type Channel =
  | { kind: "stable" }
  | { kind: "dev"; stableURL?: string }
  | { kind: "preview"; branch: string; stableURL?: string };

export function channel(env: Record<string, string | undefined> = process.env): Channel {
  if (env.VERCEL_ENV !== "preview") return { kind: "stable" };
  // The production domain, without a scheme, such as docs.example.com.
  const host = env.VERCEL_PROJECT_PRODUCTION_URL;
  const stableURL = host ? `https://${host}/docs` : undefined;
  const branch = env.VERCEL_GIT_COMMIT_REF ?? "";
  if (branch === "dev") return { kind: "dev", stableURL };
  return { kind: "preview", branch, stableURL };
}
